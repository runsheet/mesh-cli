package node

import (
	"context"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// ChooseAccount picks the account a session of that priority runs on,
// by standing against the clock and the spending order (adr-005, trd
// §3 Capacity across accounts, goal every-subscription-lands-full-at-its-reset):
//
//   - An account that does not serve the work's priority
//     (AccountConfig.Serves) is never picked, whatever its standing: a
//     subscription reserved for urgent and high work sits idle for the
//     rest (adr-005's amendment 2026-10).
//   - An account is judged by pace on its latest reading: stopped (past
//     cfg.Stop in either window, or a status not allowed, until its
//     reset) it takes nothing; paced (past its own pace point, else
//     cfg.PaceFiveHour: LimitsConfig.For) it takes only urgent and high
//     work. An account never seen yet is the furthest behind its line
//     (Distance −1), so it still has full headroom.
//   - Subscriptions first: of those that fit, the one furthest behind
//     its time-proportional target line — StandingOf's Distance, most
//     negative first; an account ahead of its line (Distance ≥ 0) is
//     chosen only when every fitting subscription is also ahead, and
//     then the least ahead of them; a tie goes to the one listed
//     first.
//   - An api account only for urgent or high work, or when every
//     subscription serving the priority is stopped and workWaits (an item, plan, evaluation
//     or rebase is waiting; research is not work that waits). Urgent and
//     high work takes an api account before a paced subscription, so a
//     subscription's last room is not spent while money is set aside
//     for exactly that work.
//
// ok is false when nothing fits: the work is not claimed and waits.
func ChooseAccount(now time.Time, accounts []AccountConfig, readings map[string]harness.RateLimit, cfg LimitsConfig, priority string, workWaits bool) (AccountConfig, bool) {
	pressing := priority == "urgent" || priority == "high"
	type fit struct {
		a        AccountConfig
		mode     paceMode
		distance float64
	}
	var subs, apis []fit
	exhausted := true // every subscription serving it stopped (vacuously, none)
	held, holding := AccountConfig{}, false
	if priority != "urgent" {
		var serving []AccountConfig
		for _, a := range accounts {
			if a.Kind == AccountSubscription && a.Serves(priority) {
				serving = append(serving, a)
			}
		}
		held, holding = heldBack(now, serving, readings, cfg)
	}
	for _, a := range accounts {
		if !a.Serves(priority) {
			continue
		}
		f := fit{a: a, mode: paceNormal, distance: -1} // never seen: furthest behind
		if r, seen := readings[a.Label]; seen {
			f.mode = pace(now, r, cfg.For(a)).mode
			f.distance = StandingOf(now, r, cfg.For(a)).Distance
		}
		if a.Kind == AccountSubscription && f.mode != paceStopped {
			exhausted = false
		}
		if holding && a.Label == held.Label {
			continue // kept for urgent work; not stopped, so exhausted stands
		}
		if f.mode == paceStopped || (f.mode == pacePaced && !pressing) {
			continue
		}
		if a.Kind == AccountAPI {
			apis = append(apis, f)
		} else {
			subs = append(subs, f)
		}
	}
	best := func(fits []fit, mode paceMode) (AccountConfig, bool) {
		var out *fit
		for i := range fits {
			if f := &fits[i]; f.mode == mode && (out == nil || f.distance < out.distance) {
				out = f
			}
		}
		if out == nil {
			return AccountConfig{}, false
		}
		return out.a, true
	}
	if a, ok := best(subs, paceNormal); ok {
		return a, true
	}
	if pressing || (exhausted && workWaits) {
		for _, mode := range []paceMode{paceNormal, pacePaced} {
			if a, ok := best(apis, mode); ok {
				return a, true
			}
		}
	}
	return best(subs, pacePaced) // only a pressing session keeps a paced one
}

// holdbackMargin is how near cfg.Stop a subscription's use must be for
// it to count toward the holdback (heldBack).
const holdbackMargin = 0.10

// heldBack is the subscription kept for urgent work (goal
// every-subscription-lands-full-at-its-reset, item 4): among subs — the
// subscriptions serving the work's priority — when there are two or
// more and every one but one has used(now, r) >= cfg.Stop − 0.10, the
// one with the latest reset: its seven-day ResetsAt, else its
// five-hour one with no seven-day reading; the last listed on a tie.
// An account never read is not within 10% of stop. ok is false when
// nothing is held back.
func heldBack(now time.Time, subs []AccountConfig, readings map[string]harness.RateLimit, cfg LimitsConfig) (AccountConfig, bool) {
	if len(subs) < 2 {
		return AccountConfig{}, false
	}
	near := cfg.Thresholds().Stop - holdbackMargin
	within := 0
	var latest time.Time
	var out AccountConfig
	for i, a := range subs {
		r, seen := readings[a.Label]
		if seen && used(now, r) >= near {
			within++
		}
		if reset := latestReset(r); i == 0 || !reset.Before(latest) {
			latest, out = reset, a
		}
	}
	if within < len(subs)-1 {
		return AccountConfig{}, false
	}
	return out, true
}

// latestReset is the reset the holdback ranks a reading by: its
// seven-day window's, else its five-hour one's; zero for none.
func latestReset(r harness.RateLimit) time.Time {
	if !r.SevenDay.ResetsAt.IsZero() {
		return r.SevenDay.ResetsAt
	}
	return r.FiveHour.ResetsAt
}

// used is how much of its plan a reading says is spent: the larger of
// its two windows, a window whose reset has come counting for nothing.
func used(now time.Time, r harness.RateLimit) float64 {
	live := func(w harness.RateLimitWindow) float64 {
		if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) {
			return 0
		}
		return w.Utilization
	}
	return max(live(r.FiveHour), live(r.SevenDay))
}

type accountKey struct{}

// WithAccount is ctx carrying the account the loop chose for the
// session it starts, for its label (runsOn, the heartbeat's slots);
// the session opens the issued one Credentials.Open chooses.
func WithAccount(ctx context.Context, a AccountConfig) context.Context {
	return context.WithValue(ctx, accountKey{}, a)
}

// ChosenAccount is the account ctx carries, if the loop chose one.
func ChosenAccount(ctx context.Context) (AccountConfig, bool) {
	a, ok := ctx.Value(accountKey{}).(AccountConfig)
	return a, ok
}

type chooserKey struct{}

// withChooser is ctx carrying the rule the loop chose the session's
// account by — its priority, whether work waits, whether it finishes
// work already built — bound to the latest readings, so that
// Credentials.Open applies the same rule among the credentials the
// platform issues the claimed session.
func withChooser(ctx context.Context, choose func([]AccountConfig) (AccountConfig, bool)) context.Context {
	return context.WithValue(ctx, chooserKey{}, choose)
}

func chooserFrom(ctx context.Context) func([]AccountConfig) (AccountConfig, bool) {
	f, _ := ctx.Value(chooserKey{}).(func([]AccountConfig) (AccountConfig, bool))
	return f
}

type priorityKey struct{}

// withPriority is ctx carrying the priority of the work a session is
// for — its goal's, "" for research — so that Credentials.Open, given
// no chooser (withChooser), still never opens an issued credential
// that does not serve it (adr-005's amendment 2026-10-02): a session
// the loop starts without choosing an account (an evaluation on Codex
// room that falls back to Claude, a held item adopted at start) keeps
// its priority.
func withPriority(ctx context.Context, priority string) context.Context {
	return context.WithValue(ctx, priorityKey{}, priority)
}

// priorityFrom is the priority ctx carries; "" (any credential) when
// none.
func priorityFrom(ctx context.Context) string {
	p, _ := ctx.Value(priorityKey{}).(string)
	return p
}
