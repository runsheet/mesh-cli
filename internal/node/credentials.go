package node

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"sort"
	"strings"
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
	// NoCredential remembers the projects whose read issued no
	// credential for an adapter (ErrNoCredential), for the loop to leave
	// their work unclaimed; the same store as Loop.NoCredential (nil:
	// not remembered, and each one logged).
	NoCredential *NoCredentials
	// Now is the clock (nil: time.Now).
	Now func() time.Time
	Log *log.Logger

	projMu sync.Mutex
	// projects are the project slugs read by id this run (projectSlug).
	projects map[string]string

	credOnce, usageOnce, toolUsageOnce, toolListOnce, usageDetailOnce, contextOnce sync.Once
	// documentsFetchedOnce and toolNamesOnce are Finish's once-per-run
	// notices for ErrNoDocumentsFetched and ErrNoToolNames (adr-016's
	// amendment 2026-10-02, goal context-is-fetched-not-shipped).
	documentsFetchedOnce, toolNamesOnce sync.Once
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
	// Model is the model the session was asked to run on: the report's
	// when the session's own usage names none (Codex's stream does not).
	Model string
	// report is the session's usage report once built (Report), res the
	// result it was built from; handedIn and noLedger are what its
	// finishing command made of it (Carried), and into the session whose
	// report carries this one's (Carries). noToolList is a report that
	// landed without its per-tool list (ErrNoToolUsage), and noDetail
	// one that landed without its turn or tool-result figures
	// (ErrNoUsageDetail); Finish logs each instead. noContext is one
	// that landed without its context bytes (ErrNoContextUsage).
	// noDocumentsFetched and noToolNames are one that landed without its
	// documents-fetched count or its per-tool call names
	// (ErrNoDocumentsFetched, ErrNoToolNames; adr-016's amendment
	// 2026-10-02).
	report                                                                               *gateway.UsageReport
	res                                                                                  harness.Result
	handedIn, noLedger, noToolList, noDetail, noContext, noDocumentsFetched, noToolNames bool
	into                                                                                 *SessionAccount
	// contextBytes is what the session's context cost on disk
	// (SetContextBytes, adr-016), the report's ContextBytes.
	contextBytes int64
	// logLabel and kind are the session's usual node.log label ("item
	// <goal>/<seq>", "goal <slug>", "project <slug>") and its kind
	// (build, evaluate, rebase, plan, research), which Finish's record
	// line carries (SetLabel, adr-016's amendment); never on the report.
	logLabel, kind string
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
// hard stop), and none left is ErrNoAccount. A read that issues none of
// the adapter's at all is ErrNoCredential, naming the org and project
// withSubject put on ctx: the session never falls to node.yaml's
// accounts or the operator's own login (adr-005's amendment 2026-10).
// Only a platform without the read leaves node.yaml's accounts
// (OpenAccount), and only for a Claude session; a Codex session has no
// such fallback — no issued Codex credential is ErrNoAccount, never the
// operator's own Codex login (ADR 011's amendment 2026-10-02). A refusal
// is an error. No error or log line carries a secret.
func (c *Credentials) Open(ctx context.Context, ref gateway.SessionRef, nodeID string, fallback []AccountConfig, adapter string) (*SessionAccount, error) {
	if adapter == "" {
		adapter = AdapterClaudeCode
	}
	if adapter == AdapterCodex {
		// node.yaml's accounts are Claude's, and a Codex session runs
		// only on an issued credential
		fallback = nil
	}
	if c == nil {
		if adapter == AdapterCodex {
			return nil, ErrNoAccount
		}
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
	unsupported := gateway.IsUnsupported(err)
	switch {
	case unsupported:
		c.credOnce.Do(func() {
			c.logf("credentials: the platform issues no session credentials; sessions run on node.yaml's accounts (said once, for every session this run)")
		})
		issued = nil
	case err != nil:
		return nil, fmt.Errorf("session credentials: %w", err)
	}
	if !unsupported {
		// the read answered: only what it issued for this adapter may
		// run the session, never the fallback nor the harness's own
		// account (adr-005's amendment 2026-10)
		subj := subjectFrom(ctx)
		if !slices.ContainsFunc(issued, func(cr gateway.Credential) bool { return IssuedAccount(cr).AdapterOf() == adapter }) {
			for _, cr := range issued {
				if err := c.Known.Note(IssuedAccount(cr), c.now()); err != nil {
					c.logf("known accounts: %v", err)
				}
			}
			nc := &ErrNoCredential{Org: cmp.Or(subj.org, "(unknown)"), Project: c.projectSlug(ctx, subj), ProjectID: subj.projectID, Adapter: adapter}
			if c.NoCredential != nil {
				c.NoCredential.Note(cmp.Or(ref.GoalID, ref.DirectionID, ref.ItemID), nc)
			} else {
				c.logf("accounts: %v", nc)
			}
			return nil, nc
		}
		c.NoCredential.Clear(subj.projectID, adapter)
	}
	if len(issued) == 0 && adapter == AdapterCodex {
		return nil, ErrNoAccount
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

// projectSlug is the subject's project slug: as the caller had it, else
// read by id once per run, else (no id, or the read failed) its id.
func (c *Credentials) projectSlug(ctx context.Context, subj subject) string {
	if subj.project != "" {
		return subj.project
	}
	if subj.projectID == "" {
		return "(unknown)"
	}
	c.projMu.Lock()
	slug, ok := c.projects[subj.projectID]
	c.projMu.Unlock()
	if ok {
		return slug
	}
	pr, ok := c.GW.(interface {
		Project(ctx context.Context, id string) (*gateway.Project, error)
	})
	if !ok {
		return subj.projectID
	}
	p, err := pr.Project(ctx, subj.projectID)
	if err != nil || p == nil || p.Slug == "" {
		return subj.projectID // not cached: the next one asks again
	}
	c.projMu.Lock()
	if c.projects == nil {
		c.projects = map[string]string{}
	}
	c.projects[subj.projectID] = p.Slug
	c.projMu.Unlock()
	return p.Slug
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
		WallMs: c.now().Sub(s.started).Milliseconds(), ContextBytes: s.contextBytes,
	}
	if r.WallMs < 0 {
		r.WallMs = 0
	}
	s.report = r
	s.Add(res)
	if r.Model == "" {
		r.Model = s.Model
	}
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
// The model is the first one read; the per-tool rows (Tools) are merged
// by tool, their calls, errors and ms summed and the first error kept,
// redacted of the account's and the served tools' secrets. Nothing
// before Report.
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
	rows := res.ToolReport()
	for i := range rows {
		if rows[i].Error != "" {
			rows[i].Error = capRunes(SessionTools{Use: s.used}.redactAllValues(s.Redact(rows[i].Error)), 200)
		}
	}
	r.Tools = mergeToolCalls(r.Tools, rows)
	r.DocumentsFetched += int64(res.DocumentsFetched)
	r.ToolNames = mergeToolNames(r.ToolNames, res.ToolNames)
	if res.Turns > 0 {
		sum := r.InputPerTurnMean*r.Turns + res.InputPerTurnMean*int64(res.Turns)
		r.Turns += int64(res.Turns)
		r.InputPerTurnMean = sum / r.Turns
		if res.InputPerTurnMax > r.InputPerTurnMax {
			r.InputPerTurnMax = res.InputPerTurnMax
		}
	}
	r.LargestToolResults = mergeLargestToolResults(r.LargestToolResults, res.LargestToolResults)
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

// SetContextBytes is what WriteContext put on disk for the session
// (ContextWritten.Bytes), carried on its usage report as ContextBytes
// (adr-016) — on the report already built, too. Nil-safe.
func (s *SessionAccount) SetContextBytes(n int) {
	if s == nil {
		return
	}
	s.contextBytes = int64(n)
	if s.report != nil {
		s.report.ContextBytes = s.contextBytes
	}
}

// SetLabel is the session's usual node.log label ("item <goal>/<seq>",
// "goal <slug>", "project <slug>") and its kind (build, evaluate,
// rebase, plan, research): Finish logs the usage record on them, as
// `<label>: usage <kind> {…}` (adr-016's amendment). They go on the log
// line only, never on the report the platform takes. Nil-safe.
func (s *SessionAccount) SetLabel(label, kind string) {
	if s == nil {
		return
	}
	s.logLabel, s.kind = label, kind
}

// Carried is the finishing command's answer to the report it carried
// (err from FinishWorkItem, EvaluateWorkItem, PlanGoal,
// FinishWorkItemRebase or RecordDirectionResearch). ErrNoUsageLedger is
// a command that landed without it: nil to the caller, and Finish logs
// the record. ErrNoToolUsage is a command that landed with the report
// but without its per-tool list: nil to the caller too, and Finish
// logs the list. ErrNoUsageDetail is a command that landed with the
// rest of the report but none of its turn or tool-result figures: also
// nil to the caller, and Finish says so once. ErrNoDocumentsFetched and
// ErrNoToolNames are the same for the documents-fetched count and the
// per-tool call names (adr-016's amendment 2026-10-02) — each may come
// joined with the others, and errors.Is finds every one of them in a
// joined error. Anything else is the command's own error.
func (s *SessionAccount) Carried(err error) error {
	noLedger := errors.Is(err, gateway.ErrNoUsageLedger)
	noToolList := !noLedger && errors.Is(err, gateway.ErrNoToolUsage)
	noDetail := !noLedger && errors.Is(err, gateway.ErrNoUsageDetail)
	noContext := !noLedger && errors.Is(err, gateway.ErrNoContextUsage)
	noDocumentsFetched := !noLedger && errors.Is(err, gateway.ErrNoDocumentsFetched)
	noToolNames := !noLedger && errors.Is(err, gateway.ErrNoToolNames)
	if s != nil && s.report != nil {
		switch {
		case noLedger:
			s.noLedger = true
		case noToolList:
			s.handedIn, s.noToolList = true, true
		case err == nil, noDetail, noContext, noDocumentsFetched, noToolNames:
			s.handedIn = true
		}
		if noDetail {
			s.noDetail = true
		}
		if noContext {
			s.noContext = true
		}
		if noDocumentsFetched {
			s.noDocumentsFetched = true
		}
		if noToolNames {
			s.noToolNames = true
		}
	}
	if noLedger || noToolList || noDetail || noContext || noDocumentsFetched || noToolNames {
		return nil
	}
	return err
}

// UseNote is SessionTools.UseNote for the tools rendered for the session
// (UseTools), redacted by the account: "" when there were none, or
// every one was called cleanly, or s is nil (an adopted session).
func (s *SessionAccount) UseNote(res harness.Result) string {
	if s == nil {
		return ""
	}
	return SessionTools{Use: s.used}.UseNote(res, s.Redact)
}

// mergeToolCalls is rows with more folded in by tool — calls, errors
// and ms summed, the first non-empty error kept — sorted by tool; nil
// when both are empty.
func mergeToolCalls(rows []gateway.ToolCallReport, more []harness.ToolCall) []gateway.ToolCallReport {
	for _, c := range more {
		i := slices.IndexFunc(rows, func(r gateway.ToolCallReport) bool { return r.Tool == c.Tool })
		if i < 0 {
			rows = append(rows, gateway.ToolCallReport{Tool: c.Tool})
			i = len(rows) - 1
		}
		rows[i].Calls += int64(c.Calls)
		rows[i].Errors += int64(c.Errors)
		rows[i].Ms += c.Ms
		if rows[i].Error == "" {
			rows[i].Error = c.Error
		}
	}
	slices.SortFunc(rows, func(a, b gateway.ToolCallReport) int { return strings.Compare(a.Tool, b.Tool) })
	return rows
}

// mergeToolNames adds more's calls per <server>__<tool> name into
// rows, summed by name (goal context-is-fetched-not-shipped).
func mergeToolNames(rows []gateway.ToolNameCount, more map[string]int) []gateway.ToolNameCount {
	for name, n := range more {
		i := slices.IndexFunc(rows, func(r gateway.ToolNameCount) bool { return r.Name == name })
		if i < 0 {
			rows = append(rows, gateway.ToolNameCount{Name: name})
			i = len(rows) - 1
		}
		rows[i].Calls += int64(n)
	}
	slices.SortFunc(rows, func(a, b gateway.ToolNameCount) int { return strings.Compare(a.Name, b.Name) })
	return rows
}

// capRunes is s cut to at most n characters.
func capRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Finish is the once-per-session end, called however the session
// ended: one `usage: turns=… input/turn mean=… max=… largest=…` line
// names the session's own per-turn input and largest tool results,
// whatever became of the report; each call of a priced tool it made
// goes to recordToolUsage (reportToolUsage); a platform whose usage
// report took no per-tool list (ErrNoToolUsage) or no turn or
// tool-result detail (ErrNoUsageDetail), or whose finishing command
// takes no `usage` (ErrNoUsageLedger), is said once per run; and the
// record itself — the report exactly as sent, redacted — is logged as
// one `<label>: usage <kind> {…}` line (SetLabel; `usage: {…}` for a
// session with none), whatever became of it: carried, refused or never
// sent, node.log holds every session's record for `mesh node usage`
// (adr-016's amendment). A session whose report another carries
// (Carries) logs none: its figures are in the carrier's record, and
// logging both would count them twice.
func (s *SessionAccount) Finish(ctx context.Context, res harness.Result) {
	if s == nil || s.creds == nil {
		return
	}
	c := s.creds
	c.logf("usage: %s", turnUsageSummary(res))
	rep := s.Report(res)
	s.reportToolUsage(ctx)
	if s.ref.ItemID == "" {
		// a plan or research session has no item to note its tools on
		if note := s.UseNote(s.res); note != "" {
			c.logf("%s", note)
		}
	}
	if s.noToolList && len(rep.Tools) > 0 {
		c.toolListOnce.Do(func() {
			c.logf("usage: the platform takes no per-tool calls yet; they are logged here instead (said once, for every session this run)")
		})
		line, _ := json.Marshal(rep.Tools)
		c.logf("usage: session %s tools: %s", rep.SessionID, s.Redact(string(line)))
	}
	if s.noDetail {
		c.usageDetailOnce.Do(func() {
			c.logf("usage: the platform's usage report carries no turns or tool-result detail; sent without them (said once, for every session this run)")
		})
	}
	if s.noContext {
		c.contextOnce.Do(func() {
			c.logf("usage: the platform takes no context bytes on the usage report yet; sent without them (said once, for every session this run)")
		})
	}
	if s.noDocumentsFetched {
		c.documentsFetchedOnce.Do(func() {
			c.logf("usage: the platform takes no documents-fetched count on the usage report yet; sent without it (said once, for every session this run)")
		})
	}
	if s.noToolNames {
		c.toolNamesOnce.Do(func() {
			c.logf("usage: the platform takes no per-tool call names on the usage report yet; sent without them (said once, for every session this run)")
		})
	}
	if s.into != nil {
		return
	}
	if s.noLedger {
		c.usageOnce.Do(func() {
			c.logf("usage: the platform's finishing command takes no usage report; each session's record is logged here instead (said once, for every session this run)")
		})
	}
	line, _ := json.Marshal(rep)
	c.logf("%s", s.recordLine(s.Redact(string(line))))
}

// recordLine is Finish's record line for the report's JSON: on the
// session's label with its kind, or the older unlabelled `usage: {…}`
// when SetLabel was never called.
func (s *SessionAccount) recordLine(js string) string {
	if s.logLabel == "" {
		return "usage: " + js
	}
	kind := s.kind
	if kind == "" {
		kind = "unknown"
	}
	return fmt.Sprintf("%s: usage %s %s", s.logLabel, kind, js)
}

// mergeLargestToolResults combines two already-sorted (largest first,
// at most five each) LargestToolResults lists into the five largest of
// the two, for a report more than one session adds to (Add).
func mergeLargestToolResults(a []gateway.ToolResultSize, b []harness.ToolResultSize) []gateway.ToolResultSize {
	merged := append([]gateway.ToolResultSize{}, a...)
	for _, t := range b {
		merged = append(merged, gateway.ToolResultSize{Tool: t.Tool, Bytes: t.Bytes})
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Bytes > merged[j].Bytes })
	if len(merged) > 5 {
		merged = merged[:5]
	}
	return merged
}

// turnUsageSummary is one session's per-turn input and largest tool
// results, plus its documents fetched (goal
// context-is-fetched-not-shipped), for Finish's node.log line.
func turnUsageSummary(res harness.Result) string {
	largest := make([]string, 0, len(res.LargestToolResults))
	for _, t := range res.LargestToolResults {
		largest = append(largest, fmt.Sprintf("%s:%d", t.Tool, t.Bytes))
	}
	return fmt.Sprintf("turns=%d input/turn mean=%d max=%d largest=%s, %d documents fetched",
		res.Turns, res.InputPerTurnMean, res.InputPerTurnMax, strings.Join(largest, ","), res.DocumentsFetched)
}

// reportToolUsage hands the platform one recordToolUsage per call of a
// priced tool the session made (trd §3 Tools, priced calls are usage),
// by the call's id: a copy of the session's report with the tool's price
// per call as its list cost, no actual cost, no tokens and none of the
// session's turn or tool-result detail (Turns, InputPerTurnMean,
// InputPerTurnMax, LargestToolResults) — those are the whole session's
// figures, not this one call's, and would be summed as if every priced
// call were its own session. An unpriced tool, one not called and a
// built-in server (mesh-context) are none: the loop is over s.used, the
// rendered console tools, which never holds the built-in whatever it is
// named — 2026-09-28, a session adopted from an older node (ADR-009) may
// still call the built-in by its old name (tools.IsBuiltin), and that is
// unpriced here too for the same reason; this note goes 2026-10-28, with
// the last node old enough to matter. A platform without recordToolUsage is said once per run and
// each call logged; a platform whose usage input lacks the detail fields
// still takes the call (ErrNoUsageDetail, folded into the once-per-run
// notice Finish already gives); a failed record is logged, never the
// item's failure.
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
			u.Turns, u.InputPerTurnMean, u.InputPerTurnMax, u.LargestToolResults = 0, 0, 0, nil
			err := c.GW.RecordToolUsage(rctx, s.ref.ItemID, id, t.Name, u)
			switch {
			case err == nil, errors.Is(err, gateway.ErrNoUsageDetail):
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
