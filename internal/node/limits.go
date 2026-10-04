package node

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// LimitsConfig is node.yaml's `limits` block: the fractions of the
// Claude plan's windows at which the node paces itself (mesh-specs trd
// §3). A key left out takes its default; a value over 1 is read as a
// percent.
type LimitsConfig struct {
	// PaceFiveHour is the five-hour window's use past which the node
	// claims only urgent and high work, at most half its capacity.
	PaceFiveHour float64 `yaml:"pace_five_hour,omitempty"`
	// Stop is either window's use past which the node starts nothing new.
	Stop float64 `yaml:"stop,omitempty"`
	// Reserve is the fraction of a window's time-proportional target
	// line held back for urgent work (Standing): the target is the
	// window's elapsed fraction less Reserve, so an account never spends
	// the clock's own cushion on ordinary work.
	Reserve float64 `yaml:"reserve,omitempty"`
	// PaceSevenDay is the seven-day window's use past which an account
	// takes only urgent and high work: never node.yaml's, only an
	// account's own pace point (For); 0 is none.
	PaceSevenDay float64 `yaml:"-"`
}

// For is the thresholds one account is paced by: its own pace points
// where it names them (AccountConfig.PaceFiveHour, PaceSevenDay — the
// credential's windowTargets), else the node-wide rule; Stop is always
// the node's.
func (c LimitsConfig) For(a AccountConfig) LimitsConfig {
	if a.PaceFiveHour > 0 {
		c.PaceFiveHour = a.PaceFiveHour
	}
	if a.PaceSevenDay > 0 {
		c.PaceSevenDay = a.PaceSevenDay
	}
	return c
}

// The thresholds when node.yaml names none.
const (
	DefaultPaceFiveHour = 0.80
	DefaultStop         = 0.98
	DefaultReserve      = 0.05
)

// Thresholds is the block with its defaults filled in.
func (c LimitsConfig) Thresholds() LimitsConfig {
	fraction := func(v, def float64) float64 {
		if v <= 0 {
			return def
		}
		if v > 1 {
			return v / 100
		}
		return v
	}
	return LimitsConfig{PaceFiveHour: fraction(c.PaceFiveHour, DefaultPaceFiveHour), Stop: fraction(c.Stop, DefaultStop),
		Reserve: fraction(c.Reserve, DefaultReserve), PaceSevenDay: fraction(c.PaceSevenDay, 0)}
}

// fiveHourLength and sevenDayLength are the two windows' own lengths,
// for Standing's elapsed fraction.
const (
	fiveHourLength = 5 * time.Hour
	sevenDayLength = 7 * 24 * time.Hour
)

// WindowStanding is one window's reading turned into the clock rule's
// terms: where it is against its own target line. Negative Distance is
// behind the line (room to spend), positive is ahead (spent too fast).
type WindowStanding struct {
	// Elapsed is the fraction of the window's length gone by, 0 to 1.
	Elapsed float64
	// Target is the line the window should be at by now: its elapsed
	// fraction less the reserve, clamped to [0, stop] — or, when the
	// account's own pace point for this window is set, that point
	// outright, in place of the time-proportional line.
	Target float64
	// Utilization is the window's live use; 0 once its reset has passed.
	Utilization float64
	// Distance is Utilization − Target: negative behind the line,
	// positive ahead of it.
	Distance float64
	// Push is set in the window's last fifth, while it is behind.
	Push bool
	// ResetsAt is the window's own reset time, as read.
	ResetsAt time.Time
}

// windowStanding is one window of a reading turned into its standing at
// now. pacePoint is the account's own pace point for this window, read
// before LimitsConfig.Thresholds fills in the node-wide pacing
// defaults — 0 is none, the ordinary case, in which the target is the
// time-proportional line; only a credential's own windowTargets (or an
// operator's explicit node.yaml pace point) past 0 replaces it outright.
// A window with no reading or whose reset has passed reads as freshly
// reset: elapsed, target, utilization and push all zero, whatever
// pacePoint says — a stale reading names no standing to push on.
func windowStanding(now time.Time, w harness.RateLimitWindow, length time.Duration, pacePoint float64, cfg LimitsConfig) WindowStanding {
	if w.ResetsAt.IsZero() || !w.ResetsAt.After(now) {
		return WindowStanding{ResetsAt: w.ResetsAt}
	}
	elapsed := clampUnit(1 - w.ResetsAt.Sub(now).Seconds()/length.Seconds())
	target := clamp(elapsed-cfg.Reserve, 0, cfg.Stop)
	if pacePoint > 0 {
		target = pacePoint
	}
	distance := w.Utilization - target
	push := distance < 0 && w.ResetsAt.Sub(now) <= length/5
	return WindowStanding{Elapsed: elapsed, Target: target, Utilization: w.Utilization, Distance: distance, Push: push, ResetsAt: w.ResetsAt}
}

// clamp bounds v to [lo, hi].
func clamp(v, lo, hi float64) float64 {
	return min(max(v, lo), hi)
}

// clampUnit bounds v to [0, 1].
func clampUnit(v float64) float64 { return clamp(v, 0, 1) }

// pacePointFraction reads a pace point the same way Thresholds reads a
// threshold: 0 or less is none, over 1 is a percent.
func pacePointFraction(v float64) float64 {
	if v <= 0 {
		return 0
	}
	if v > 1 {
		return v / 100
	}
	return v
}

// Standing is a reading turned into the clock rule's terms (goal
// every-subscription-lands-full-at-its-reset, item 1): each window's own
// standing, and the account's own Distance and Push — the seven-day
// window's Distance when it has a reading, else the five-hour's; Push
// when either window's is set.
type Standing struct {
	FiveHour, SevenDay WindowStanding
	// Target is the line the account itself stands against: the
	// seven-day window's Target when it has a reading, else the
	// five-hour's — the same window Distance falls back to.
	Target float64
	// Distance is negative behind the account's own target, positive
	// ahead of it.
	Distance float64
	Push     bool
}

// StandingOf turns a reading into Standing at now, against cfg (already
// LimitsConfig.For(account), so a credential's own windowTargets reach
// this as PaceFiveHour/PaceSevenDay). The pace points are read before
// Thresholds fills in the node-wide pacing defaults (pace_five_hour's
// 0.80), so an account with no pace point of its own still gets the
// time-proportional target line on both windows, not a flat default.
func StandingOf(now time.Time, r harness.RateLimit, cfg LimitsConfig) Standing {
	fiveHourPoint, sevenDayPoint := pacePointFraction(cfg.PaceFiveHour), pacePointFraction(cfg.PaceSevenDay)
	cfg = cfg.Thresholds()
	s := Standing{
		FiveHour: windowStanding(now, r.FiveHour, fiveHourLength, fiveHourPoint, cfg),
		SevenDay: windowStanding(now, r.SevenDay, sevenDayLength, sevenDayPoint, cfg),
	}
	s.Target, s.Distance = s.FiveHour.Target, s.FiveHour.Distance
	if !r.SevenDay.ResetsAt.IsZero() {
		s.Target, s.Distance = s.SevenDay.Target, s.SevenDay.Distance
	}
	s.Push = s.FiveHour.Push || s.SevenDay.Push
	return s
}

// AccountStanding is one scheduled account's clock standing as `mesh
// node status` shows it (goal every-subscription-lands-full-at-its-reset,
// item 6): Standing's windows and overall Distance/Push beside the
// account's own Label and Kind, and Mode — "paced" or "stopped" when
// the account's own reading puts it there (pace), "" otherwise.
type AccountStanding struct {
	Label, Kind        string
	FiveHour, SevenDay WindowStanding
	Distance           float64
	Push               bool
	Mode               string
}

// StandingAccounts is the clock standing of every account among
// accounts that has a reading in readings, in label order: what the
// loop leaves beside node.scaling each tick for `mesh node status`
// (Loop.Standing). An account with no reading names no standing — it
// has spent nothing to judge.
func StandingAccounts(now time.Time, accounts []AccountConfig, readings map[string]harness.RateLimit, cfg LimitsConfig) []AccountStanding {
	out := make([]AccountStanding, 0, len(accounts))
	for _, a := range accounts {
		r, ok := readings[a.Label]
		if !ok {
			continue
		}
		acfg := cfg.For(a)
		s := StandingOf(now, r, acfg)
		mode := ""
		if p := pace(now, r, acfg); p.mode != paceNormal {
			mode = p.mode.String()
		}
		out = append(out, AccountStanding{Label: a.Label, Kind: a.Kind, FiveHour: s.FiveHour, SevenDay: s.SevenDay,
			Distance: s.Distance, Push: s.Push, Mode: mode})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// DefaultAccount names the account of a node whose sessions run on the
// CLI's own config directory.
const DefaultAccount = "default"

// Account is the Claude account a session with that env runs on: the
// label of the configured account it runs on (AccountEnv, adr-005), else
// its CLAUDE_CONFIG_DIR (the job's own when it sets one, else the
// node's), or DefaultAccount.
func Account(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], AccountEnv+"="); ok && v != "" {
			return v
		}
	}
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "CLAUDE_CONFIG_DIR="); ok && v != "" {
			return v
		}
	}
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	return DefaultAccount
}

// jobAccount is the label a session's env names (AccountEnv), or "".
func jobAccount(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], AccountEnv+"="); ok && v != "" {
			return v
		}
	}
	return ""
}

// Limits keeps the latest plan standing each account's sessions
// reported. Every kind of session feeds it; the heartbeat carries it and
// the loop paces by it.
type Limits struct {
	mu       sync.Mutex
	accounts map[string]harness.RateLimit
}

// Observe records a reading of the account, unless one observed later
// is already held.
func (s *Limits) Observe(account string, rl *harness.RateLimit) {
	if rl == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accounts == nil {
		s.accounts = map[string]harness.RateLimit{}
	}
	if held, ok := s.accounts[account]; ok && held.ObservedAt.After(rl.ObservedAt) {
		return
	}
	s.accounts[account] = *rl
}

// CodexAccountPrefix keys a Codex account's readings in Limits
// (CodexLimitsAccount): kept apart from the Claude accounts', they are
// on the heartbeat but never pace, stop or scale a Claude session
// (docs/adr/adr-005-codex-second-opinion.md).
const CodexAccountPrefix = "codex:"

// CodexLimitsAccount is the key a Codex account's readings land under:
// its label's. Every Codex session runs on an issued credential, which
// has one; there is no codex:default of the operator's own login.
func CodexLimitsAccount(name string) string {
	return CodexAccountPrefix + name
}

// Readings are the latest reading of every Claude account, by account:
// what the node paces, chooses and scales by. A Codex account's are not
// among them (Reading).
func (s *Limits) Readings() map[string]harness.RateLimit {
	out := s.all()
	for a := range out {
		if strings.HasPrefix(a, CodexAccountPrefix) {
			delete(out, a)
		}
	}
	return out
}

// CodexReadings are the latest reading of every Codex account, by its
// label (the codex: prefix taken off): what a Codex session's account is
// chosen by, apart from the Claude accounts'.
func (s *Limits) CodexReadings() map[string]harness.RateLimit {
	out := map[string]harness.RateLimit{}
	for a, rl := range s.all() {
		if label, ok := strings.CutPrefix(a, CodexAccountPrefix); ok {
			out[label] = rl
		}
	}
	return out
}

// Reading is the latest reading of one account, of any kind.
func (s *Limits) Reading(account string) (harness.RateLimit, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rl, ok := s.accounts[account]
	return rl, ok
}

func (s *Limits) all() map[string]harness.RateLimit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]harness.RateLimit, len(s.accounts))
	for a, rl := range s.accounts {
		out[a] = rl
	}
	return out
}

// Heartbeat is the readings of every account, Codex's too, as the
// heartbeat carries them, by account, each with the priorities known
// names it serves (a Codex account's own label, its "codex:" prefix
// stripped) — empty for one serving all, or one known has not heard the
// platform issue — so the Nodes page's account gauges can show what
// each serves (adr-005's amendment 2026-10, `limits[].priorities`), and
// each Claude account's standing against the clock rule's target line —
// Target, Distance, Push, from Standing(now, rl, cfg.For(account)) — a
// Codex account carrying none of the three, since the clock does not
// pace it (adr-005's amendment 2026-10-03).
func (s *Limits) Heartbeat(known []AccountConfig, now time.Time, cfg LimitsConfig) []gateway.Limits {
	priorities := make(map[string][]string, len(known))
	configs := make(map[string]AccountConfig, len(known))
	for _, a := range known {
		priorities[a.Label] = a.Priorities
		configs[a.Label] = a
	}
	readings := s.all()
	accounts := make([]string, 0, len(readings))
	for a := range readings {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	out := make([]gateway.Limits, 0, len(accounts))
	for _, a := range accounts {
		rl := readings[a]
		label := strings.TrimPrefix(a, CodexAccountPrefix)
		l := gateway.Limits{
			Account: a, Status: rl.Status, RateLimitType: rl.RateLimitType, ResetsAt: rl.ResetsAt,
			OverageStatus: rl.OverageStatus, IsUsingOverage: rl.IsUsingOverage,
			FiveHour: limitWindow(rl.FiveHour), SevenDay: limitWindow(rl.SevenDay), ObservedAt: rl.ObservedAt,
			Priorities: priorities[label],
		}
		if !strings.HasPrefix(a, CodexAccountPrefix) {
			st := StandingOf(now, rl, cfg.For(configs[label]))
			l.Target, l.Distance, l.Push = &st.Target, &st.Distance, &st.Push
		}
		out = append(out, l)
	}
	return out
}

func limitWindow(w harness.RateLimitWindow) *gateway.LimitWindow {
	if w == (harness.RateLimitWindow{}) {
		return nil
	}
	return &gateway.LimitWindow{Utilization: w.Utilization, ResetsAt: w.ResetsAt}
}

// ObserveLimits wraps a harness so every session it runs — build,
// evaluate, plan, research, rebase — reports its plan standing into
// store, from its progress and from its result, under the account its
// env runs on.
func ObserveLimits(h harness.Harness, store *Limits) harness.Harness {
	return &limitsHarness{Harness: h, store: store}
}

// ObserveLimitsAs is ObserveLimits under one fixed account key, for a
// job that names none: a Codex session on an issued credential lands
// under CodexLimitsAccount of its label all the same.
func ObserveLimitsAs(h harness.Harness, store *Limits, account string) harness.Harness {
	return &limitsHarness{Harness: h, store: store, account: account}
}

type limitsHarness struct {
	harness.Harness
	store   *Limits
	account string
}

func (h *limitsHarness) Run(ctx context.Context, job harness.Job, onProgress func(harness.Progress)) (harness.Result, error) {
	account := h.account
	if h.Harness.Name() == AdapterCodex && (jobAccount(job.Env) != "" || account == "") {
		// a Codex session on an issued account (its label on the env),
		// kept apart from the Claude accounts' readings
		account = CodexLimitsAccount(jobAccount(job.Env))
	}
	if account == "" {
		account = Account(job.Env)
	}
	res, err := h.Harness.Run(ctx, job, func(p harness.Progress) {
		h.store.Observe(account, p.RateLimit)
		if onProgress != nil {
			onProgress(p)
		}
	})
	h.store.Observe(account, res.RateLimit)
	return res, err
}

// paceMode is how much the node takes on, by its plan's standing.
type paceMode int

const (
	// paceNormal: everything, up to capacity.
	paceNormal paceMode = iota
	// pacePaced: urgent and high items and plans only, at most half the
	// capacity; no research.
	pacePaced
	// paceStopped: nothing new — no item, plan, research, rebase or
	// evaluation; what runs finishes.
	paceStopped
)

func (m paceMode) String() string {
	switch m {
	case pacePaced:
		return "paced"
	case paceStopped:
		return "stopped"
	}
	return "normal"
}

// unknownResetWait is how long a reading that stops the node holds when
// it says no reset time: a stopped node runs no session to read a newer
// one, so it tries again after this.
const unknownResetWait = 15 * time.Minute

// pacing is a decision: the mode, until when, and why.
type pacing struct {
	mode     paceMode
	resumeAt time.Time
	why      string
}

// pace decides the mode one reading puts the node in at now. Stopped:
// either window at or past cfg.Stop, or a status other than "allowed";
// it lasts until the triggering window's reset (the event's own for the
// status). Paced: the five-hour window at or past cfg.PaceFiveHour,
// or the seven-day one at or past cfg.PaceSevenDay when set (an
// account's own pace point, LimitsConfig.For), until its reset. A trigger whose reset has come is stale and counts
// for nothing, so the node resumes normal at resumeAt.
func pace(now time.Time, r harness.RateLimit, cfg LimitsConfig) pacing {
	cfg = cfg.Thresholds()
	until := func(resets time.Time) time.Time {
		if resets.IsZero() {
			return r.ObservedAt.Add(unknownResetWait)
		}
		return resets
	}
	stop := pacing{mode: paceNormal}
	trigger := func(why string, resets time.Time) {
		at := until(resets)
		if !now.Before(at) {
			return // stale: the window has started over
		}
		if at.After(stop.resumeAt) {
			stop.resumeAt = at
		}
		stop.mode = paceStopped
		if stop.why != "" {
			stop.why += "; "
		}
		stop.why += why
	}
	if r.FiveHour.Utilization >= cfg.Stop {
		trigger(fmt.Sprintf("five-hour window at %s (stop %s)", pct(r.FiveHour.Utilization), pct(cfg.Stop)), r.FiveHour.ResetsAt)
	}
	if r.SevenDay.Utilization >= cfg.Stop {
		trigger(fmt.Sprintf("seven-day window at %s (stop %s)", pct(r.SevenDay.Utilization), pct(cfg.Stop)), r.SevenDay.ResetsAt)
	}
	// "allowed" and "allowed_warning" (a window filling, the CLI's own
	// nudge) both mean sessions may run; only a refusal stops the node.
	// 2026-09-26: allowed_warning at 77% of the week read as a stop and
	// the mini sat idle with 14 items ready.
	if r.Status != "" && !strings.HasPrefix(r.Status, "allowed") {
		trigger(fmt.Sprintf("status %s", r.Status), r.ResetsAt)
	}
	if stop.mode == paceStopped {
		return stop
	}
	// allowed_warning — the CLI's nudge that a window is filling — paces
	// the node rather than stopping it: urgent and high work at half
	// capacity until that window resets.
	if strings.HasPrefix(r.Status, "allowed") && r.Status != "allowed" {
		if at := until(r.ResetsAt); now.Before(at) {
			return pacing{mode: pacePaced, resumeAt: at, why: fmt.Sprintf("status %s (pace)", r.Status)}
		}
	}
	if r.FiveHour.Utilization >= cfg.PaceFiveHour {
		if at := until(r.FiveHour.ResetsAt); now.Before(at) {
			return pacing{mode: pacePaced, resumeAt: at,
				why: fmt.Sprintf("five-hour window at %s (pace %s)", pct(r.FiveHour.Utilization), pct(cfg.PaceFiveHour))}
		}
	}
	if cfg.PaceSevenDay > 0 && r.SevenDay.Utilization >= cfg.PaceSevenDay {
		if at := until(r.SevenDay.ResetsAt); now.Before(at) {
			return pacing{mode: pacePaced, resumeAt: at,
				why: fmt.Sprintf("seven-day window at %s (pace %s)", pct(r.SevenDay.Utilization), pct(cfg.PaceSevenDay))}
		}
	}
	return pacing{mode: paceNormal}
}

// strictest is the reading of the strictest decision among readings;
// of two in one mode, the one that lasts longer. A node with no accounts
// configured runs every session on the operator's own login, whatever
// key its readings landed under, and is judged by this one.
func strictest(now time.Time, readings map[string]harness.RateLimit, cfg LimitsConfig) (harness.RateLimit, bool) {
	accounts := make([]string, 0, len(readings))
	for a := range readings {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	var out harness.RateLimit
	var outPace pacing
	for i, a := range accounts {
		p := pace(now, readings[a], cfg)
		if i == 0 || p.mode > outPace.mode || (p.mode == outPace.mode && p.resumeAt.After(outPace.resumeAt)) {
			out, outPace = readings[a], p
		}
	}
	return out, len(accounts) > 0
}

// nodePace is the node's mode, for its log and its capacity: the most
// lenient of its subscriptions' (of every account's, when it has no
// subscription) — the node is stopped only when every one of them is,
// until the first of them resets. Which account a session runs on is
// ChooseAccount's.
func nodePace(now time.Time, accounts []AccountConfig, readings map[string]harness.RateLimit, cfg LimitsConfig) pacing {
	judged := make([]AccountConfig, 0, len(accounts))
	for _, a := range accounts {
		if a.Kind != AccountAPI {
			judged = append(judged, a)
		}
	}
	if len(judged) == 0 {
		judged = accounts
	}
	var out pacing
	for i, a := range judged {
		p := pacing{mode: paceNormal}
		if r, ok := readings[a.Label]; ok {
			p = pace(now, r, cfg.For(a))
		}
		if (len(judged) > 1 || a.Label != DefaultAccount) && p.why != "" {
			p.why = a.Label + ": " + p.why
		}
		if i == 0 || p.mode < out.mode || (p.mode == out.mode && p.resumeAt.Before(out.resumeAt)) {
			out = p
		}
	}
	return out
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

// pacedFree is the room a paced node has: at most half its capacity
// (never less than one) running at once.
func pacedFree(free, capacity, running int) int {
	return min(free, max(1, capacity/2)-running)
}

// Attach implements harness.Attacher when the harness it wraps does: an
// adopted session's plan standing lands under the account its state
// file names.
func (h *limitsHarness) Attach(ctx context.Context, dir string, onProgress func(harness.Progress)) (harness.Result, error) {
	at, ok := h.Harness.(harness.Attacher)
	if !ok {
		return harness.Result{}, errNoAttach
	}
	account := h.account
	if account == "" {
		account = stateAccount(dir)
	}
	res, err := at.Attach(ctx, dir, func(p harness.Progress) {
		h.store.Observe(account, p.RateLimit)
		if onProgress != nil {
			onProgress(p)
		}
	})
	h.store.Observe(account, res.RateLimit)
	return res, err
}
