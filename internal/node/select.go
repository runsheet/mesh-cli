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
