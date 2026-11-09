package node

import (
	"context"
	"fmt"
	"math"

	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// t0 is the tests' now; the windows reset an hour and a day after it.
var (
	t0       = time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	fiveOver = t0.Add(time.Hour)
	weekOver = t0.Add(24 * time.Hour)
)

// reading is an allowed plan standing at t0 with those windows used.
func reading(five, seven float64) *harness.RateLimit {
	return &harness.RateLimit{Status: "allowed", RateLimitType: "five_hour", ResetsAt: fiveOver,
		FiveHour: harness.RateLimitWindow{Utilization: five, ResetsAt: fiveOver},
		SevenDay: harness.RateLimitWindow{Utilization: seven, ResetsAt: weekOver}, ObservedAt: t0}
}

// halfWeekOver is 3.5 days after t0: a seven-day window's reset there
// has half the week elapsed, so its clock target is 0.5 less the
// default reserve.
var halfWeekOver = t0.Add(84 * time.Hour)

// readingHalfWeek is an allowed plan standing at t0 with the seven-day
// window at that use and its reset 3.5 days out — half the week gone
// — for the clock rule's tests (goal every-subscription-lands-full-at-its-reset).
func readingHalfWeek(seven float64) *harness.RateLimit {
	return &harness.RateLimit{Status: "allowed", RateLimitType: "seven_day", ResetsAt: halfWeekOver,
		FiveHour: harness.RateLimitWindow{Utilization: 0, ResetsAt: fiveOver},
		SevenDay: harness.RateLimitWindow{Utilization: seven, ResetsAt: halfWeekOver}, ObservedAt: t0}
}

// A session that reports its standing only on its result feeds the
// store too, under the account of its env; an older reading never
// replaces a newer one.
func TestLimitsStoreKeepsTheLatestPerAccount(t *testing.T) {
	store := &Limits{}
	newer := reading(0.5, 0.2)
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		return harness.Result{Success: true, RateLimit: newer}, nil
	})
	if _, err := ObserveLimits(h, store).Run(context.Background(), harness.Job{Env: []string{"CLAUDE_CONFIG_DIR=/home/mini/.claude-max"}}, nil); err != nil {
		t.Fatal(err)
	}
	older := reading(0.9, 0.9)
	older.ObservedAt = t0.Add(-time.Minute)
	store.Observe("/home/mini/.claude-max", older)
	if got := store.Readings()["/home/mini/.claude-max"]; got.FiveHour.Utilization != 0.5 {
		t.Fatalf("the older reading won: %+v", got)
	}
	later := reading(0.6, 0.2)
	later.ObservedAt = t0.Add(time.Minute)
	store.Observe("/home/mini/.claude-max", later)
	if got := store.Readings()["/home/mini/.claude-max"]; got.FiveHour.Utilization != 0.6 {
		t.Fatalf("the later reading lost: %+v", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if a := Account(nil); a != DefaultAccount {
		t.Fatalf("Account(nil) = %q", a)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/srv/claude")
	if a := Account([]string{"GH_TOKEN=x"}); a != "/srv/claude" {
		t.Fatalf("Account = %q, want the node's own", a)
	}
}

// pace, the pure decision, at the defaults and at the edges.
func TestPaceDecides(t *testing.T) {
	status := func(s string) harness.RateLimit {
		r := *reading(0.1, 0.1)
		r.Status, r.ResetsAt = s, t0.Add(30*time.Minute)
		return r
	}
	for _, c := range []struct {
		name   string
		r      harness.RateLimit
		cfg    LimitsConfig
		now    time.Time
		mode   paceMode
		resume time.Time
	}{
		{"low", *reading(0.5, 0.5), LimitsConfig{}, t0, paceNormal, time.Time{}},
		{"five-hour at 80%", *reading(0.80, 0.5), LimitsConfig{}, t0, pacePaced, fiveOver},
		{"five-hour at 98%", *reading(0.98, 0.5), LimitsConfig{}, t0, paceStopped, fiveOver},
		{"seven-day at 99%", *reading(0.85, 0.99), LimitsConfig{}, t0, paceStopped, weekOver},
		{"allowed_warning", status("allowed_warning"), LimitsConfig{}, t0, pacePaced, t0.Add(30 * time.Minute)},
		{"rejected", status("rejected"), LimitsConfig{}, t0, paceStopped, t0.Add(30 * time.Minute)},
		{"paced past its reset", *reading(0.85, 0.5), LimitsConfig{}, fiveOver, paceNormal, time.Time{}},
		{"seven-day past its reset", *reading(0.1, 0.99), LimitsConfig{}, weekOver, paceNormal, time.Time{}},
		{"stopped by the week, five-hour over", *reading(0.99, 0.99), LimitsConfig{}, fiveOver, paceStopped, weekOver},
		{"lower pace", *reading(0.6, 0.1), LimitsConfig{PaceFiveHour: 0.5}, t0, pacePaced, fiveOver},
		{"lower stop", *reading(0.6, 0.1), LimitsConfig{PaceFiveHour: 0.5, Stop: 0.6}, t0, paceStopped, fiveOver},
		{"percent", *reading(0.6, 0.1), LimitsConfig{PaceFiveHour: 50}, t0, pacePaced, fiveOver},
	} {
		got := pace(c.now, c.r, c.cfg)
		if got.mode != c.mode || !got.resumeAt.Equal(c.resume) {
			t.Errorf("%s: pace = %v until %v (%s), want %v until %v", c.name, got.mode, got.resumeAt, got.why, c.mode, c.resume)
		}
	}
	// no reset time given: a stop holds for unknownResetWait, then the
	// node tries a session again
	r := status("rejected")
	r.ResetsAt = time.Time{}
	if got := pace(t0, r, LimitsConfig{}); got.mode != paceStopped || !got.resumeAt.Equal(t0.Add(unknownResetWait)) {
		t.Fatalf("no resetsAt: %v until %v", got.mode, got.resumeAt)
	}
}

// approx is whether a and b are within 1e-9 of each other: the clock's
// own float arithmetic, not a flaky comparison.
func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// StandingOf, the pure clock: each window's elapsed fraction, target
// line, distance from it and push, against the seven-day window — the
// window every example names, since it is the one the goal balances.
// Both windows take the time-proportional target by default; only a
// pace point of their own (a credential's windowTargets, or an
// explicit node.yaml one) replaces it.
func TestStanding(t *testing.T) {
	length := sevenDayLength
	at := func(fraction float64) time.Time { return t0.Add(time.Duration(fraction * float64(length))) }

	for _, c := range []struct {
		name                                  string
		resetsAt                              time.Time
		use                                   float64
		cfg                                   LimitsConfig
		wantElapsed, wantTarget, wantDistance float64
		wantPush                              bool
	}{
		{"50% elapsed, 10% use", at(0.5), 0.10, LimitsConfig{}, 0.5, 0.45, -0.35, false},
		{"10% left, 60% use: push", at(0.1), 0.60, LimitsConfig{}, 0.9, 0.85, -0.25, true},
		{"10% left, 90% use: ahead, no push", at(0.1), 0.90, LimitsConfig{}, 0.9, 0.85, 0.05, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := harness.RateLimit{SevenDay: harness.RateLimitWindow{Utilization: c.use, ResetsAt: c.resetsAt}}
			s := StandingOf(t0, r, c.cfg)
			if !approx(s.SevenDay.Elapsed, c.wantElapsed) || !approx(s.SevenDay.Target, c.wantTarget) ||
				!approx(s.SevenDay.Distance, c.wantDistance) || s.SevenDay.Push != c.wantPush {
				t.Fatalf("seven-day = %+v, want elapsed %v target %v distance %v push %v",
					s.SevenDay, c.wantElapsed, c.wantTarget, c.wantDistance, c.wantPush)
			}
		})
	}

	// a credential's own pace point replaces the time-proportional line
	// outright, whatever the elapsed fraction.
	t.Run("pace point overrides the line", func(t *testing.T) {
		r := harness.RateLimit{SevenDay: harness.RateLimitWindow{Utilization: 0.2, ResetsAt: at(0.3)}}
		s := StandingOf(t0, r, LimitsConfig{PaceSevenDay: 0.5})
		if !approx(s.SevenDay.Target, 0.5) {
			t.Fatalf("target = %v, want 0.5 regardless of elapsed", s.SevenDay.Target)
		}
	})

	// a window whose reset has passed reads as freshly reset: elapsed,
	// target and utilization all zero, not the stale percentage.
	t.Run("past its reset", func(t *testing.T) {
		r := harness.RateLimit{SevenDay: harness.RateLimitWindow{Utilization: 0.77, ResetsAt: t0.Add(-time.Hour)}}
		s := StandingOf(t0, r, LimitsConfig{})
		if s.SevenDay.Elapsed != 0 || s.SevenDay.Target != 0 || s.SevenDay.Utilization != 0 {
			t.Fatalf("past reset = %+v, want elapsed/target/utilization all 0", s.SevenDay)
		}
	})

	// with no seven-day reading the account's own Distance is the
	// five-hour window's.
	t.Run("no seven-day reading", func(t *testing.T) {
		r := harness.RateLimit{FiveHour: harness.RateLimitWindow{Utilization: 0.3, ResetsAt: t0.Add(2 * time.Hour)}}
		s := StandingOf(t0, r, LimitsConfig{})
		if !approx(s.Distance, s.FiveHour.Distance) {
			t.Fatalf("account distance = %v, want the five-hour window's %v", s.Distance, s.FiveHour.Distance)
		}
	})

	// the five-hour window takes the time-proportional target by default
	// too — pace_five_hour's flat 0.80 (LimitsConfig.Thresholds) paces
	// the node's old brake, never Standing's line, when no pace point of
	// its own is set.
	t.Run("five-hour window, no pace point: time-proportional, not the flat pace default", func(t *testing.T) {
		r := harness.RateLimit{FiveHour: harness.RateLimitWindow{Utilization: 0.10, ResetsAt: t0.Add(fiveHourLength / 2)}}
		s := StandingOf(t0, r, LimitsConfig{})
		if !approx(s.FiveHour.Elapsed, 0.5) || !approx(s.FiveHour.Target, 0.45) || !approx(s.FiveHour.Distance, -0.35) {
			t.Fatalf("five-hour = %+v, want elapsed 0.5 target 0.45 distance -0.35 (not the flat 0.80 pace default)", s.FiveHour)
		}
	})

	// a pace point on an expired window is not retained: a stale reading
	// stands at zero, never the pace point, and never pushes.
	t.Run("pace point on a window past its reset: zero, no push", func(t *testing.T) {
		r := harness.RateLimit{SevenDay: harness.RateLimitWindow{Utilization: 0.9, ResetsAt: t0.Add(-time.Hour)}}
		s := StandingOf(t0, r, LimitsConfig{PaceSevenDay: 0.5})
		if s.SevenDay.Elapsed != 0 || s.SevenDay.Target != 0 || s.SevenDay.Utilization != 0 || s.SevenDay.Push {
			t.Fatalf("past reset with a pace point = %+v, want elapsed/target/utilization 0 and no push", s.SevenDay)
		}
	})
}

// The heartbeat gives each Claude account its standing against the
// clock's target line (adr-005's amendment 2026-10-03,
// `limits[].target`, `distance`, `push`); a Codex account carries none
// of the three, since the clock does not pace it.
func TestHeartbeatStanding(t *testing.T) {
	limits := &Limits{}
	limits.Observe("max-cloud", &harness.RateLimit{Status: "allowed",
		SevenDay: harness.RateLimitWindow{Utilization: 0.10, ResetsAt: t0.Add(sevenDayLength / 2)}, ObservedAt: t0})
	limits.Observe(CodexLimitsAccount("chatgpt-pro"), &harness.RateLimit{Status: "allowed",
		FiveHour: harness.RateLimitWindow{Utilization: 0.5, ResetsAt: t0.Add(time.Hour)}, ObservedAt: t0})

	byAccount := map[string]gateway.Limits{}
	for _, l := range limits.Heartbeat(nil, t0, LimitsConfig{}) {
		byAccount[l.Account] = l
	}
	claude := byAccount["max-cloud"]
	if claude.Target == nil || claude.Distance == nil || claude.Push == nil {
		t.Fatalf("claude account carries nil standing: %+v", claude)
	}
	if !approx(*claude.Target, 0.45) || !approx(*claude.Distance, -0.35) || *claude.Push {
		t.Fatalf("standing = target %v distance %v push %v, want 0.45 -0.35 false", *claude.Target, *claude.Distance, *claude.Push)
	}
	codex := byAccount["codex:chatgpt-pro"]
	if codex.Target != nil || codex.Distance != nil || codex.Push != nil {
		t.Fatalf("codex account carries standing: %+v", codex)
	}
}

// allowed_warning is the CLI saying a window is filling, not a refusal:
// the node keeps running on it.
func TestPaceAllowedWarningIsNotAStop(t *testing.T) {
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	r := harness.RateLimit{Status: "allowed_warning", ObservedAt: now,
		FiveHour: harness.RateLimitWindow{Utilization: 0.05, ResetsAt: now.Add(4 * time.Hour)},
		SevenDay: harness.RateLimitWindow{Utilization: 0.77, ResetsAt: now.Add(56 * time.Hour)}}
	if p := pace(now, r, LimitsConfig{}); p.mode != pacePaced {
		t.Fatalf("allowed_warning at 5%%/77%% decided %v (%s), want paced", p.mode, p.why)
	}
	r.Status = "rejected"
	if p := pace(now, r, LimitsConfig{}); p.mode != paceStopped {
		t.Fatalf("a rejected status did not stop: %v", p.mode)
	}
}

// issuedPair is a store that knows the two subscriptions the platform
// issued the mini, max20 and shhemail, noted at t0.
func issuedPair(t *testing.T, path string) *KnownAccounts {
	t.Helper()
	known := &KnownAccounts{Path: path}
	for i, label := range []string{"max20", "shhemail"} {
		if err := known.Note(AccountConfig{Label: label, Kind: AccountSubscription, ID: fmt.Sprintf("cred-%d", i+1)}, t0); err != nil {
			t.Fatal(err)
		}
	}
	return known
}
