package node

import (
	"context"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// ChooseAccount picks the account a session of that priority runs on,
// by headroom and the spending order (adr-005, trd §3 Capacity across
// accounts):
//
//   - An account that does not serve the work's priority
//     (AccountConfig.Serves) is never picked, whatever its headroom: a
//     subscription reserved for urgent and high work sits idle for the
//     rest (adr-005's amendment 2026-10).
//   - An account is judged by pace on its latest reading: stopped (past
//     cfg.Stop in either window, or a status not allowed, until its
//     reset) it takes nothing; paced (past its own pace point, else
//     cfg.PaceFiveHour: LimitsConfig.For) it takes only urgent and high
//     work. An account never seen yet has its full
//     headroom.
//   - Subscriptions first: of those that fit, the one with the most
//     headroom — the lowest of max(five-hour, seven-day) use, a window
//     past its reset counting as unused; a tie goes to the one listed
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
		a    AccountConfig
		mode paceMode
		used float64
	}
	var subs, apis []fit
	exhausted := true // every subscription serving it stopped (vacuously, none)
	for _, a := range accounts {
		if !a.Serves(priority) {
			continue
		}
		f := fit{a: a, mode: paceNormal}
		if r, seen := readings[a.Label]; seen {
			f.mode, f.used = pace(now, r, cfg.For(a)).mode, used(now, r)
		}
		if a.Kind == AccountSubscription && f.mode != paceStopped {
			exhausted = false
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
			if f := &fits[i]; f.mode == mode && (out == nil || f.used < out.used) {
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
