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
	SessionCredentials(ctx context.Context, ref gateway.SessionRef, nodeID string) ([]gateway.Credential, error)
	RecordSessionUsage(ctx context.Context, rec gateway.UsageRecord) error
}

// Credentials is where every session's account comes from and where its
// usage goes (adr-005, the claim-time read): the platform's
// sessionCredentials for the claimed session first, node.yaml's
// accounts when the platform does not know the read or issues none; a
// usage record per session to recordSessionUsage, or one structured log
// line on a platform without it. Each fallback is said once per run.
// A nil *Credentials is the node before the platform half: node.yaml's
// accounts and no usage record.
type Credentials struct {
	GW CredentialGateway
	// Limits is the node's latest reading per account, for the windows
	// before and after a session and for choosing among the issued.
	Limits *Limits
	// Thresholds are node.yaml's limits (nil: the defaults).
	Thresholds func() LimitsConfig
	// Now is the clock (nil: time.Now).
	Now func() time.Time
	Log *log.Logger

	credOnce, usageOnce sync.Once
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
	before    *gateway.UsageWindows
}

// Open is the account a claimed session runs on. It asks the platform
// for the session's credentials (the claim-time read); those it issues
// are the accounts the session chooses among, by the rule the loop chose
// by (withChooser) — an api credential whose daily or monthly cap is
// spent is never among them (a hard stop), and none left is
// ErrNoAccount. A platform without the read, or one that issues
// nothing, leaves node.yaml's accounts (OpenAccount); a refusal is an
// error. No error or log line carries a secret.
func (c *Credentials) Open(ctx context.Context, ref gateway.SessionRef, nodeID string, fallback []AccountConfig) (*SessionAccount, error) {
	if c == nil {
		acct, err := OpenAccount(ctx, fallback)
		if err != nil {
			return nil, err
		}
		return &SessionAccount{AccountSession: acct}, nil
	}
	s := &SessionAccount{creds: c, ref: ref, nodeID: nodeID, started: c.now()}
	issued, err := c.GW.SessionCredentials(ctx, ref, nodeID)
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
				return ChooseAccount(c.now(), accts, c.readings(), c.thresholds(), "", true)
			}
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
	if r, ok := c.readings()[s.label()]; ok {
		s.before = usageWindows(&r)
	}
	return s, nil
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
	if a, ok := ChosenAccount(ctx); ok {
		return a
	}
	return accounts[0]
}

// IssuedAccount is a platform credential as an account a session may
// run on: its label (its id when it has none), kind and id, its secret
// held in memory for this session only.
func IssuedAccount(cr gateway.Credential) AccountConfig {
	label := cr.Label
	if label == "" {
		label = cr.ID
	}
	secret := cr.Secret
	return AccountConfig{Label: label, Kind: cr.Kind, ID: cr.ID, issued: &secret}
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

// label is the account the session's limits are kept under.
func (s *SessionAccount) label() string {
	if s.account.Label != "" {
		return s.account.Label
	}
	return DefaultAccount
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

// Finish reports the session's usage record: its credential, tokens,
// cost, the windows before and after, and when it ran — to the
// platform's ledger, or as one `usage: {…}` log line on a platform
// without it. It is called once a session has run, however it ended.
func (s *SessionAccount) Finish(ctx context.Context, res harness.Result) {
	if s == nil || s.creds == nil {
		return
	}
	c := s.creds
	rec := gateway.UsageRecord{
		Kind: s.ref.Kind, ItemID: s.ref.ItemID, GoalID: s.ref.GoalID, DirectionID: s.ref.DirectionID,
		NodeID: s.nodeID, SessionID: res.SessionID,
		CredentialID: s.account.ID, CredentialLabel: s.label(), CredentialKind: s.account.Kind,
		CostUSD: res.CostUSD, Before: s.before, StartedAt: s.started.UTC(), EndedAt: c.now().UTC(),
	}
	if rec.CredentialKind == "" {
		rec.CredentialKind = AccountSubscription // the operator's own login
	}
	if u := res.Usage; u != nil {
		rec.Model = u.Model
		rec.InputTokens, rec.OutputTokens = u.InputTokens, u.OutputTokens
		rec.CacheReadTokens, rec.CacheWriteTokens = u.CachedInputTokens, u.CacheWriteInputTokens
	}
	if res.RateLimit != nil {
		rec.After = usageWindows(res.RateLimit)
	} else if r, ok := c.readings()[s.label()]; ok {
		rec.After = usageWindows(&r)
	}
	// the session has run and spent: its record outlives a shutdown
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err := c.GW.RecordSessionUsage(rctx, rec)
	if err == nil {
		return
	}
	if gateway.IsUnsupported(err) {
		c.usageOnce.Do(func() {
			c.logf("usage: the platform keeps no usage ledger; each session's record is logged here instead (said once, for every session this run)")
		})
	} else {
		c.logf("usage: record: %v", s.RedactErr(err))
	}
	line, _ := json.Marshal(rec)
	c.logf("usage: %s", s.Redact(string(line)))
}

func usageWindows(r *harness.RateLimit) *gateway.UsageWindows {
	w := &gateway.UsageWindows{FiveHour: limitWindow(r.FiveHour), SevenDay: limitWindow(r.SevenDay)}
	if w.FiveHour == nil && w.SevenDay == nil {
		return nil
	}
	return w
}
