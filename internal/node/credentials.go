package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/tools"
)

// An api key's caps when the platform names none (runsheet ADR 018 (e)).
const (
	DefaultCapDailyUSD   = 50.0
	DefaultCapMonthlyUSD = 1000.0
)

// ErrNoAccount is a claimed session none of whose platform credentials
// may take it: every one stopped, paced for its priority or, for an api
// key, at its cap. The session does not start; a build releases its
// item rather than failing it.
var ErrNoAccount = errors.New("no issued credential has headroom for the session")

// CredentialGateway is what the claim-time read and the usage ledger
// need of the platform; *gateway.Client is one.
type CredentialGateway interface {
	Session(ctx context.Context, ref gateway.SessionRef, nodeID string) (*gateway.Session, error)
	RecordToolUsage(ctx context.Context, itemID, toolCallID, tool string, usage gateway.UsageReport) error
}

// Credentials is where every session's account comes from and where its
// usage goes (adr-005, the claim-time read): the platform's read for
// the claimed session (itemCredentials, planCredentials or
// researchCredentials, with its tools) first, node.yaml's
// accounts when the platform does not know the read or issues none; a
// usage report per session on the command that ends it (Report,
// Carried), or one structured log line when that command has nowhere to
// put it (Finish). Each fallback is said once per run.
// A nil *Credentials is the node before the platform half: node.yaml's
// accounts and no usage record.
type Credentials struct {
	GW CredentialGateway
	// Limits is the node's latest reading per account, for the windows
	// before and after a session and for choosing among the issued.
	Limits *Limits
	// Thresholds are node.yaml's limits (nil: the defaults).
	Thresholds func() LimitsConfig
	// Known remembers every account the claim-time read hands a session
	// (label, kind, id, caps; never the secret), so the loop can learn
	// which accounts the platform issues (nil: not remembered here).
	Known *KnownAccounts
	// Now is the clock (nil: time.Now).
	Now func() time.Time
	Log *log.Logger

	credOnce, usageOnce, toolUsageOnce sync.Once
}

func (c *Credentials) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Credentials) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log.Printf(format, args...)
	}
}

func (c *Credentials) readings() map[string]harness.RateLimit {
	if c.Limits == nil {
		return nil
	}
	return c.Limits.Readings()
}

// SessionAccount is one session's hold on its account — the
// AccountSession it runs with — and what its usage record needs: the
// session it is, the credential, when it started and the windows then.
type SessionAccount struct {
	*AccountSession
	creds   *Credentials
	ref     gateway.SessionRef
	nodeID  string
	account AccountConfig
	// remaining is an api credential's room under its caps (0: none known).
	remaining float64
	started   time.Time
	before    *harness.RateLimit
	// Adapter is the harness the session runs on, as its report names
	// it ("claude-code" when empty; Adapter).
	Adapter string
	// report is the session's usage report once built (Report), res the
	// result it was built from; handedIn and noLedger are what its
	// finishing command made of it (Carried), and into the session whose
	// report carries this one's (Carries).
	report             *gateway.UsageReport
	res                harness.Result
	handedIn, noLedger bool
	into               *SessionAccount
	// Tools are the tools the claim-time read handed the session, their
	// secrets decrypted (nil from a platform without them); what the
	// session gets of them is SessionTools'.
	Tools []tools.Tool
	// toolEnv is the tool secrets put on the session (ApplyTools), which
	// Redact takes out too.
	toolEnv []string
	// used are the tools rendered for the session (SessionTools.Use), whose
	// priced calls Finish reports.
	used []tools.Tool
}

// Open is the account a claimed session on adapter (Adapter of its
// harness: "claude-code" or "codex"; empty is claude-code) runs on. It
// asks the platform for the session's credentials (the claim-time
// read); those it issues for that adapter are the accounts the session
// chooses among, by the rule the loop chose by (withChooser) — an api
// credential whose daily or monthly cap is spent is never among them (a
// hard stop), and none left, or none of the adapter's issued at all, is
// ErrNoAccount. A platform without the read, or one that issues
// nothing, leaves node.yaml's accounts (OpenAccount) — for a Codex
// session, the Codex harness's own account (no AccountSession); a
// refusal is an error. No error or log line carries a secret.
func (c *Credentials) Open(ctx context.Context, ref gateway.SessionRef, nodeID string, fallback []AccountConfig, adapter string) (*SessionAccount, error) {
	if adapter == "" {
		adapter = AdapterClaudeCode
	}
	if adapter == AdapterCodex {
		// node.yaml's accounts are Claude's; the codex: block's is the
		// harness's own
		fallback = nil
	}
	if c == nil {
		acct, err := OpenAccount(ctx, fallback)
		if err != nil {
			return nil, err
		}
		return &SessionAccount{AccountSession: acct, Adapter: adapter}, nil
	}
	s := &SessionAccount{creds: c, ref: ref, nodeID: nodeID, started: c.now(), Adapter: adapter}
	var issued []gateway.Credential
	read, err := c.GW.Session(ctx, ref, nodeID)
	if err == nil && read != nil {
		issued, s.Tools = read.Credentials, read.Tools
	}
	switch {
	case gateway.IsUnsupported(err):
		c.credOnce.Do(func() {
			c.logf("credentials: the platform issues no session credentials; sessions run on node.yaml's accounts (said once, for every session this run)")
		})
		issued = nil
	case err != nil:
		return nil, fmt.Errorf("session credentials: %w", err)
	}
	if len(issued) == 0 {
		acct, err := OpenAccount(ctx, fallback)
		if err != nil {
			return nil, err
		}
		s.AccountSession = acct
		s.account = openedAccount(ctx, fallback)
	} else {
		var accounts []AccountConfig
		room := map[string]float64{}
		for _, cr := range issued {
			a := IssuedAccount(cr)
			if err := c.Known.Note(a, c.now()); err != nil {
				c.logf("known accounts: %v", err)
			}
			if a.AdapterOf() != adapter {
				continue
			}
			if a.Kind == AccountAPI {
				left := CapRoom(cr)
				if left <= 0 {
					c.logf("credentials: %s is at its cap ($%.2f of $%.2f today, $%.2f of $%.2f this month); not used", cr.Label, cr.SpentDailyUSD, capOr(cr.CapDailyUSD, DefaultCapDailyUSD), cr.SpentMonthlyUSD, capOr(cr.CapMonthlyUSD, DefaultCapMonthlyUSD))
					continue
				}
				room[a.Label] = left
			}
			accounts = append(accounts, a)
		}
		choose := chooserFrom(ctx)
		if choose == nil {
			choose = func(accts []AccountConfig) (AccountConfig, bool) {
				return ChooseAccount(c.now(), accts, c.readingsFor(adapter), c.thresholds(), "", true)
			}
		}
		if len(accounts) == 0 {
			return nil, ErrNoAccount
		}
		a, ok := choose(accounts)
		if !ok {
			return nil, ErrNoAccount
		}
		secret, err := a.Secret()
		if err != nil {
			return nil, err
		}
		acct, err := PrepareAccount(a, secret)
		if err != nil {
			return nil, err
		}
		s.AccountSession, s.account, s.remaining = acct, a, room[a.Label]
	}
	if r, ok := c.reading(s.limitsKey()); ok {
		s.before = &r
	}
	return s, nil
}

// readingsFor are the readings an adapter's accounts are judged by, by
// label: the Claude accounts' (Readings), or the Codex accounts' with
// their codex: prefix taken off (Limits.CodexReadings).
func (c *Credentials) readingsFor(adapter string) map[string]harness.RateLimit {
	if adapter != AdapterCodex {
		return c.readings()
	}
	if c.Limits == nil {
		return nil
	}
	return c.Limits.CodexReadings()
}

func (c *Credentials) reading(key string) (harness.RateLimit, bool) {
	if c.Limits == nil {
		return harness.RateLimit{}, false
	}
	return c.Limits.Reading(key)
}

func (c *Credentials) thresholds() LimitsConfig {
	if c.Thresholds != nil {
		return c.Thresholds()
	}
	return LimitsConfig{}
}

// openedAccount is the node.yaml account OpenAccount opened: the chosen
// one, else the first, else none (the operator's own login).
func openedAccount(ctx context.Context, accounts []AccountConfig) AccountConfig {
	if len(accounts) == 0 {
		return AccountConfig{}
	}
	return chosenAmong(ctx, accounts)
}

// IssuedAccount is a platform credential as an account a session may
// run on: its label (its id when it has none), kind, id and caps, its
// secret held in memory for this session only.
func IssuedAccount(cr gateway.Credential) AccountConfig {
	label := cr.Label
	if label == "" {
		label = cr.ID
	}
	secret := cr.Secret
	adapter := cr.Adapter
	if adapter == AdapterClaudeCode {
		adapter = ""
	}
	return AccountConfig{Label: label, Kind: cr.Kind, ID: cr.ID, Adapter: adapter, SecretVersion: cr.SecretVersion,
		DailyCapUSD: cr.CapDailyUSD, MonthlyCapUSD: cr.CapMonthlyUSD, issued: &secret}
}

// CapRoom is how many dollars an api credential may still spend: the
// smaller of what its daily and its monthly cap leave (a cap of 0 is the
// default, $50 and $1,000). Zero or less is a spent cap — the hard stop.
func CapRoom(cr gateway.Credential) float64 {
	day := capOr(cr.CapDailyUSD, DefaultCapDailyUSD) - cr.SpentDailyUSD
	month := capOr(cr.CapMonthlyUSD, DefaultCapMonthlyUSD) - cr.SpentMonthlyUSD
	return math.Min(day, month)
}

func capOr(v, def float64) float64 {
	if v > 0 {
		return v
	}
	return def
}

// label is the account the session runs on.
func (s *SessionAccount) label() string {
	if s.account.Label != "" {
		return s.account.Label
	}
	return DefaultAccount
}

// limitsKey is the key the session's limits are kept under in Limits:
// the label, or for a Codex session CodexLimitsAccount of it.
func (s *SessionAccount) limitsKey() string {
	if s.Adapter == AdapterCodex {
		return CodexLimitsAccount(s.account.Label)
	}
	return s.label()
}

// Apply puts the account on the job, as AccountSession.Apply does, and
// holds an api credential's session to what its cap leaves: a session
// does not continue past the cap.
func (s *SessionAccount) Apply(job *harness.Job) {
	if s == nil {
		return
	}
	s.AccountSession.Apply(job)
	if s.remaining > 0 && (job.MaxBudgetUSD <= 0 || job.MaxBudgetUSD > s.remaining) {
		job.MaxBudgetUSD = s.remaining
	}
}

// Redact is text with the account's secret and every tool secret the
// session carries taken out.
func (s *SessionAccount) Redact(text string) string {
	if s == nil {
		return text
	}
	return tools.Redact(s.AccountSession.Redact(text), s.toolEnv)
}

// RedactErr is err with the account's and the tools' secrets taken out,
// still wrapping err when there was nothing to take out.
func (s *SessionAccount) RedactErr(err error) error {
	if err == nil || s == nil {
		return err
	}
	err = s.AccountSession.RedactErr(err)
	if msg := err.Error(); tools.Redact(msg, s.toolEnv) != msg {
		return errors.New(tools.Redact(msg, s.toolEnv))
	}
	return err
}

// Adapter is the report's adapter for a harness: "codex" for the Codex
// harness, "claude-code" for everything else.
func Adapter(h harness.Harness) string {
	if h != nil && h.Name() == "codex" {
		return "codex"
	}
	return "claude-code"
}

// Report is the session's usage report, the one its finishing command
// carries as `usage` (adr-005, the claim-time read and the usage
// record): the session, its adapter and model, the credential the
// platform issued it (its id, sent only when set) and its kind, tokens,
// tool calls, wall time, the list cost and — on an api credential — the
// actual cost in cents, and the account's windows before and after, each
// left out when never read. Never the secret. The first call builds it
// (the session's end); later calls, and Add, work on that one. Nil when
// the session had no account (an adopted one) or the node has no
// Credentials.
func (s *SessionAccount) Report(res harness.Result) *gateway.UsageReport {
	if s == nil || s.creds == nil {
		return nil
	}
	if s.report != nil {
		return s.report
	}
	c := s.creds
	s.res = res
	kind := gateway.CredentialUnset
	switch s.account.Kind {
	case AccountSubscription:
		kind = gateway.CredentialSubscription
	case AccountAPI:
		kind = gateway.CredentialAPI
	}
	adapter := s.Adapter
	if adapter == "" {
		adapter = "claude-code"
	}
	r := &gateway.UsageReport{
		SessionID: res.SessionID, Adapter: adapter,
		CredentialID: s.account.ID, CredentialKind: kind,
		WallMs: c.now().Sub(s.started).Milliseconds(),
	}
	if r.WallMs < 0 {
		r.WallMs = 0
	}
	s.report = r
	s.Add(res)
	if s.before != nil {
		r.FiveHourBefore, r.FiveHourResetsAtBefore = reportWindow(s.before.FiveHour)
		r.SevenDayBefore, r.SevenDayResetsAtBefore = reportWindow(s.before.SevenDay)
	}
	after := res.RateLimit
	if after == nil {
		if rl, ok := c.reading(s.limitsKey()); ok {
			after = &rl
		}
	}
	if after != nil {
		r.FiveHourAfter, r.FiveHourResetsAtAfter = reportWindow(after.FiveHour)
		r.SevenDayAfter, r.SevenDayResetsAtAfter = reportWindow(after.SevenDay)
	}
	return r
}

// Add puts another session's tokens, tool calls and cost into the
// session's report, for a finishing command that ends more than one
// session: the evaluation's Codex second opinion, a plan asked twice.
// The model is the first one read. Nothing before Report.
func (s *SessionAccount) Add(res harness.Result) {
	if s == nil || s.report == nil {
		return
	}
	r := s.report
	if u := res.Usage; u != nil {
		if r.Model == "" {
			r.Model = u.Model
		}
		r.InputTokens += u.InputTokens
		r.OutputTokens += u.OutputTokens
		r.CacheReadTokens += u.CachedInputTokens
		r.CacheWriteTokens += u.CacheWriteInputTokens
	}
	for _, n := range res.ToolCalls {
		r.ToolCalls += int64(n)
	}
	cents := int64(Cents(res.CostUSD))
	r.ListCostCents += cents
	if r.CredentialKind == gateway.CredentialAPI {
		r.ActualCostCents += cents
	}
}

// Carries is another session whose report is folded into this one's
// (Add) and handed in on this one's finishing command: its Finish still
// reports its priced tool calls, and leaves the record to this one's.
func (s *SessionAccount) Carries(other *SessionAccount) {
	if s == nil || other == nil || other == s {
		return
	}
	other.Report(other.res)
	s.Add(other.res)
	other.into = s
}

// Carried is the finishing command's answer to the report it carried
// (err from FinishWorkItem, EvaluateWorkItem, PlanGoal,
// FinishWorkItemRebase or RecordDirectionResearch). ErrNoUsageLedger is
// a command that landed without it: nil to the caller, and Finish logs
// the record. Anything else is the command's own error.
func (s *SessionAccount) Carried(err error) error {
	noLedger := errors.Is(err, gateway.ErrNoUsageLedger)
	if s != nil && s.report != nil {
		switch {
		case err == nil:
			s.handedIn = true
		case noLedger:
			s.noLedger = true
		}
	}
	if noLedger {
		return nil
	}
	return err
}

// Finish is the once-per-session end, called however the session
// ended: each call of a priced tool it made goes to recordToolUsage
// (reportToolUsage), and a report its finishing command did not carry —
// a failed item or rebase, a plan or research error, or a platform
// whose command takes no `usage` (ErrNoUsageLedger, said once per run)
// — is logged as one `usage: {…}` line: the session spent, and its
// record must exist somewhere.
func (s *SessionAccount) Finish(ctx context.Context, res harness.Result) {
	if s == nil || s.creds == nil {
		return
	}
	c := s.creds
	rep := s.Report(res)
	s.reportToolUsage(ctx)
	if s.handedIn || s.into != nil {
		return
	}
	if s.noLedger {
		c.usageOnce.Do(func() {
			c.logf("usage: the platform's finishing command takes no usage report; each session's record is logged here instead (said once, for every session this run)")
		})
	}
	line, _ := json.Marshal(rep)
	c.logf("usage: %s", s.Redact(string(line)))
}

// reportToolUsage hands the platform one recordToolUsage per call of a
// priced tool the session made (trd §3 Tools, priced calls are usage),
// by the call's id: a copy of the session's report with the tool's price
// per call as its list cost, no actual cost and no tokens. An unpriced
// tool, one not called and a built-in server (mesh-context) are none. A
// platform without recordToolUsage is said once per run and each call
// logged; a failed record is logged, never the item's failure.
func (s *SessionAccount) reportToolUsage(ctx context.Context) {
	c := s.creds
	if s.report == nil {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	for _, t := range s.used {
		if t.PricePerCallCents <= 0 {
			continue
		}
		for _, id := range s.res.ToolCallIDs[t.Name] {
			u := *s.report
			u.ListCostCents, u.ActualCostCents = int64(t.PricePerCallCents), 0
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens = 0, 0, 0, 0
			u.ToolCalls = 1
			err := c.GW.RecordToolUsage(rctx, s.ref.ItemID, id, t.Name, u)
			switch {
			case err == nil:
				continue
			case gateway.IsUnsupported(err):
				c.toolUsageOnce.Do(func() {
					c.logf("usage: the platform takes no tool usage records; priced tool calls are logged here instead (said once, for every session this run)")
				})
			default:
				c.logf("usage: tool %s: %v", t.Name, s.RedactErr(err))
			}
			c.logf("usage: tool %s on item %s (%s): call %s, %d¢", t.Name, s.ref.ItemID, s.ref.Kind, id, t.PricePerCallCents)
		}
	}
}

// reportWindow is one window's utilization and reset as a report's fields;
// nil for a window never read, and a reset only when one was given.
func reportWindow(w harness.RateLimitWindow) (*float64, *time.Time) {
	if w == (harness.RateLimitWindow{}) {
		return nil, nil
	}
	u := w.Utilization
	if w.ResetsAt.IsZero() {
		return &u, nil
	}
	at := w.ResetsAt.UTC()
	return &u, &at
}
