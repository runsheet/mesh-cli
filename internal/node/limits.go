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
}

// The thresholds when node.yaml names none.
const (
	DefaultPaceFiveHour = 0.80
	DefaultStop         = 0.95
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
	return LimitsConfig{PaceFiveHour: fraction(c.PaceFiveHour, DefaultPaceFiveHour), Stop: fraction(c.Stop, DefaultStop)}
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

// Readings are the latest reading of every account, by account.
func (s *Limits) Readings() map[string]harness.RateLimit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]harness.RateLimit, len(s.accounts))
	for a, rl := range s.accounts {
		out[a] = rl
	}
	return out
}

// Heartbeat is the readings as the heartbeat carries them, by account.
func (s *Limits) Heartbeat() []gateway.Limits {
	readings := s.Readings()
	accounts := make([]string, 0, len(readings))
	for a := range readings {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	out := make([]gateway.Limits, 0, len(accounts))
	for _, a := range accounts {
		rl := readings[a]
		out = append(out, gateway.Limits{
			Account: a, Status: rl.Status, RateLimitType: rl.RateLimitType, ResetsAt: rl.ResetsAt,
			OverageStatus: rl.OverageStatus, IsUsingOverage: rl.IsUsingOverage,
			FiveHour: limitWindow(rl.FiveHour), SevenDay: limitWindow(rl.SevenDay), ObservedAt: rl.ObservedAt,
		})
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

type limitsHarness struct {
	harness.Harness
	store *Limits
}

func (h *limitsHarness) Run(ctx context.Context, job harness.Job, onProgress func(harness.Progress)) (harness.Result, error) {
	account := Account(job.Env)
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
// until its reset. A trigger whose reset has come is stale and counts
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
	if r.Status != "" && r.Status != "allowed" {
		trigger(fmt.Sprintf("status %s", r.Status), r.ResetsAt)
	}
	if stop.mode == paceStopped {
		return stop
	}
	if r.FiveHour.Utilization >= cfg.PaceFiveHour {
		if at := until(r.FiveHour.ResetsAt); now.Before(at) {
			return pacing{mode: pacePaced, resumeAt: at,
				why: fmt.Sprintf("five-hour window at %s (pace %s)", pct(r.FiveHour.Utilization), pct(cfg.PaceFiveHour))}
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
			p = pace(now, r, cfg)
		}
		if len(judged) > 1 && p.why != "" {
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
