package node

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
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

// clock is a settable Loop.Now.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Set(t time.Time) { c.mu.Lock(); c.now = t; c.mu.Unlock() }

// pacedLoop is priorityFake's loop over both orgs at t0 with a reading
// already in its store.
func (p *priorityFake) pacedLoop(capacity int, rl *harness.RateLimit) (*Loop, *strings.Builder, *clock) {
	p.register()
	loop, logs := p.loop(capacity)
	c := &clock{now: t0}
	loop.Now = c.Now
	loop.Limits = &Limits{}
	loop.Limits.Observe(DefaultAccount, rl)
	return loop, logs, c
}

// A reading a session reports in its progress — and in nothing else —
// lands in the store under the session's account and goes out on the
// next heartbeat.
func TestLimitsFromProgressReachTheHeartbeat(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node"}); err != nil {
		t.Fatal(err)
	}
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		on(harness.Progress{SessionID: "s-1"})
		on(harness.Progress{SessionID: "s-1", RateLimit: reading(0.42, 0.17)})
		return harness.Result{Success: true, Text: "Done.", CostUSD: 0.5, SessionID: "s-1"}, nil
	})
	ws := &fakeWS{root: t.TempDir()}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Harness: "claude-code", Limits: &Limits{}, Now: func() time.Time { return t0 }}
	runner := &Runner{Credentials: testCreds(gw), GW: gw, Harness: ObserveLimits(h, loop.Limits), WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour}
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) }

	loop.Tick(context.Background())
	loop.wg.Wait()
	if hb := f.CallsNamed("heartbeatNode"); len(hb) != 1 || hb[0].Input["limits"] != nil {
		t.Fatalf("no session has run yet, no limits to send: %+v", hb)
	}
	got := loop.Limits.Readings()["max-issued"]
	if got.FiveHour.Utilization != 0.42 || got.SevenDay.Utilization != 0.17 {
		t.Fatalf("store = %+v", loop.Limits.Readings())
	}

	loop.Tick(context.Background())
	hb := f.CallsNamed("heartbeatNode")
	limits, _ := hb[len(hb)-1].Input["limits"].([]any)
	if len(limits) != 1 {
		t.Fatalf("heartbeat limits = %#v", hb[len(hb)-1].Input["limits"])
	}
	l := limits[0].(map[string]any)
	five, _ := l["fiveHour"].(map[string]any)
	seven, _ := l["sevenDay"].(map[string]any)
	if l["account"] != "max-issued" || l["status"] != "allowed" || five["utilization"] != 0.42 || seven["utilization"] != 0.17 ||
		five["resetsAt"] != "2026-09-25T15:00:00Z" || l["observedAt"] != "2026-09-25T14:00:00Z" {
		t.Fatalf("heartbeat limits = %#v", l)
	}
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

// Past 80% of the five-hour window, with capacity 4, one normal and
// three urgent/high items ready: only urgent and high are claimed, at
// most 2 — acme's best and globex's, each org below its floor of 2
// (ADR-023); only the urgent/high goal is planned; no research.
func TestLoopPacedClaimsUrgentAndHighAtHalfCapacity(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addGoal(orgID, "fire", "urgent")
	p.addGoal(org2, "hot", "high")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	p.addItem(orgID, "fire", "urgent-1", 2, false)
	p.addItem(orgID, "fire", "urgent-2", 3, false)
	p.addItem(org2, "hot", "high-1", 1, false)
	p.addPlanning(orgID, "plan-normal", "normal")
	p.addPlanning(orgID, "plan-high", "high")
	direction(p.Fake, dirID, projID, "nightly", "Tidy up.", 5)
	loop, logs, _ := p.pacedLoop(4, reading(0.85, 0.3))

	claimed := p.claimedBy(loop)
	if strings.Join(claimed, ",") != "urgent-1,high-1" {
		t.Fatalf("paced claimed %v, want urgent-1,high-1", claimed)
	}
	if got := started(logs.String(), "planning goal"); strings.Join(got, ",") != "plan-high" {
		t.Fatalf("paced planned %v, want plan-high only", got)
	}
	if n := len(p.CallsNamed("claimDirectionResearch")); n != 0 {
		t.Fatalf("paced claimed research %d times", n)
	}
	if !strings.Contains(logs.String(), "limits: paced until 2026-09-25T15:00:00Z: five-hour window at 85% (pace 80%)") {
		t.Fatalf("the crossing is not logged:\n%s", logs)
	}
}

// At 96% of the seven-day window, or with the status not allowed, the
// node claims no item, plans nothing, researches nothing, rebases
// nothing and starts no evaluation — while the build it runs goes on,
// uncancelled. Once the clock passes the reset it claims again.
func TestLoopStoppedStartsNothingAndResumesAtReset(t *testing.T) {
	stoppers := map[string]func() *harness.RateLimit{
		"seven-day 99%": func() *harness.RateLimit { return reading(0.3, 0.99) },
		"rejected":      func() *harness.RateLimit { r := reading(0.3, 0.3); r.Status = "rejected"; return r },
	}
	for name, stopper := range stoppers {
		t.Run(name, func(t *testing.T) {
			p := newPriorityFake(t)
			p.addGoal(orgID, "g", "urgent")
			p.addItem(orgID, "g", "long-one", 1, false)
			loop, logs, c := p.pacedLoop(3, reading(0.1, 0.1))
			release := make(chan struct{})
			defer close(release)
			var cancelled bool
			var mu sync.Mutex
			loop.Dispatch = func(ctx context.Context, _ gateway.WorkItem) {
				select {
				case <-release:
				case <-ctx.Done():
					mu.Lock()
					cancelled = true
					mu.Unlock()
				}
			}
			var rebased int
			loop.Rebase = func(context.Context, gateway.WorkItem) { rebased++ }
			loop.Tick(context.Background())
			if loop.Running() != 1 {
				t.Fatalf("running = %d before the stop", loop.Running())
			}

			// the stop arrives, and with it work of every kind
			rl := stopper()
			rl.ObservedAt = t0.Add(time.Second)
			loop.Limits.Observe(DefaultAccount, rl)
			p.addItem(orgID, "g", "next-one", 2, false)
			p.addPlanning(orgID, "planned", "urgent")
			p.addHeld(orgID, "g", "held-one", 3)
			p.addItem(orgID, "g", "conflicted", 4, false)
			p.WorkItems["conflicted"]["status"], p.WorkItems["conflicted"]["rebaseState"] = "review", "requested"
			direction(p.Fake, dirID, projID, "nightly", "Tidy up.", 5)
			claims := len(p.CallsNamed("claimWorkItem"))
			loop.Tick(context.Background())
			loop.Tick(context.Background())
			if n := len(p.CallsNamed("claimWorkItem")); n != claims {
				t.Fatalf("a stopped node claimed %d item(s)", n-claims)
			}
			if n := len(p.CallsNamed("claimPlan")) + len(p.CallsNamed("claimDirectionResearch")) + len(p.CallsNamed("claimWorkItemRebase")); n != 0 || rebased != 0 {
				t.Fatalf("a stopped node claimed %d plan/research/rebase(s)", n)
			}
			if got := started(logs.String(), "evaluating item"); len(got) != 0 {
				t.Fatalf("a stopped node started evaluations %v", got)
			}
			mu.Lock()
			wasCancelled := cancelled
			mu.Unlock()
			if loop.Running() != 1 || wasCancelled {
				t.Fatalf("the running build must go on: running=%d cancelled=%v", loop.Running(), wasCancelled)
			}
			if !strings.Contains(logs.String(), "limits: stopped until ") || !strings.Contains(logs.String(), "1 session(s) to finish") {
				t.Fatalf("the stop is not logged:\n%s", logs)
			}

			// past the reset the reading is stale: normal again
			c.Set(weekOver.Add(time.Second))
			loop.Tick(context.Background())
			if n := len(p.CallsNamed("claimWorkItem")); n <= claims {
				t.Fatalf("past the reset the node must claim again\n%s", logs)
			}
			if p.Item("next-one")["status"] != "claimed" {
				t.Fatalf("next-one = %v", p.Item("next-one")["status"])
			}
			if !strings.Contains(logs.String(), "limits: back to normal (was stopped)") {
				t.Fatalf("the resume is not logged:\n%s", logs)
			}
		})
	}
}

// The thresholds come from node.yaml, read every tick: at pace_five_hour
// 0.5 a five-hour window at 60% paces the node (the normal item waits);
// with the block gone, the defaults leave it normal.
func TestLoopTakesLimitsFromNodeYAML(t *testing.T) {
	p := newPriorityFake(t)
	p.register()
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 2\nlimits:\n  pace_five_hour: 0.5\n  stop: 0.9\n")
	loop.Now = func() time.Time { return t0 }
	loop.Limits = &Limits{}
	loop.Limits.Observe(DefaultAccount, reading(0.6, 0.1))
	if got := p.claimedBy(loop); len(got) != 0 {
		t.Fatalf("paced at 0.5, claimed %v", got)
	}
	if th := loop.Thresholds(); th.PaceFiveHour != 0.5 || th.Stop != 0.9 {
		t.Fatalf("thresholds = %+v", th)
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 2\n")
	if got := p.claimedBy(loop); strings.Join(got, ",") != "normal-1" {
		t.Fatalf("at the defaults, claimed %v", got)
	}
	for _, want := range []string{"limits: paced until", "limits: pace five-hour 50% → 80%, stop 90% → 98%", "limits: back to normal (was paced)"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, logs)
		}
	}
	if th := (LimitsConfig{}).Thresholds(); th.PaceFiveHour != DefaultPaceFiveHour || th.Stop != DefaultStop || th.Reserve != DefaultReserve {
		t.Fatalf("defaults = %+v", th)
	}
}

// LimitsConfig's defaults — reserve 0.05, stop 0.98 — and node.yaml's
// own values, a percent or a fraction, loaded through LoadConfig and the
// live reload alike.
func TestLimitsDefaults(t *testing.T) {
	if th := (LimitsConfig{}).Thresholds(); th.Reserve != 0.05 || th.Stop != 0.98 {
		t.Fatalf("defaults = %+v, want reserve 0.05, stop 0.98", th)
	}
	if th := (LimitsConfig{Reserve: 10}).Thresholds(); th.Reserve != 0.10 {
		t.Fatalf("reserve: 10 = %v, want 0.10 (read as a percent)", th.Reserve)
	}

	p := newPriorityFake(t)
	p.register()
	loop, _, _ := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\nlimits:\n  reserve: 0.10\n  stop: 0.97\n")
	loop.Tick(context.Background())
	loop.wg.Wait()
	if th := loop.Thresholds(); th.Reserve != 0.10 || th.Stop != 0.97 {
		t.Fatalf("thresholds = %+v, want reserve 0.10, stop 0.97", th)
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

// Ticks in one mode log its crossing once: into paced, into stopped,
// back to normal — each a single line however many ticks it lasts.
func TestLoopLogsEachCrossingOnce(t *testing.T) {
	p := newPriorityFake(t)
	loop, logs, c := p.pacedLoop(2, reading(0.85, 0.1))
	for range 3 {
		loop.Tick(context.Background())
	}
	stop := reading(0.99, 0.1)
	stop.ObservedAt = t0.Add(time.Second)
	loop.Limits.Observe(DefaultAccount, stop)
	for range 3 {
		loop.Tick(context.Background())
	}
	c.Set(fiveOver)
	for range 3 {
		loop.Tick(context.Background())
	}
	loop.wg.Wait()
	for _, line := range []string{"limits: paced until", "limits: stopped until 2026-09-25T15:00:00Z", "limits: back to normal"} {
		if n := strings.Count(logs.String(), line); n != 1 {
			t.Fatalf("%q logged %d times:\n%s", line, n, logs)
		}
	}
}

// Without a reading the node is normal and its heartbeat carries no limits.
func TestLoopWithoutReadingsIsNormal(t *testing.T) {
	p := newPriorityFake(t)
	p.register()
	p.addGoal(orgID, "steady", "low")
	p.addItem(orgID, "steady", "low-1", 1, false)
	loop, logs := p.loop(1)
	if got := p.claimedBy(loop); strings.Join(got, ",") != "low-1" {
		t.Fatalf("claimed %v", got)
	}
	for _, hb := range p.CallsNamed("heartbeatNode") {
		if hb.Input["limits"] != nil {
			t.Fatalf("heartbeat with no readings: %+v", hb.Input)
		}
	}
	if strings.Contains(logs.String(), "limits:") {
		t.Fatalf("no crossing to log:\n%s", logs)
	}
}

// Evaluations and rebases finish work already built: a paced account
// still takes them at normal priority, as pacing always did, while a
// stopped one takes neither.
func TestLoopPacedStillEvaluatesAndRebases(t *testing.T) {
	for _, c := range []struct {
		name   string
		rl     *harness.RateLimit
		starts bool
	}{
		{"paced", reading(0.85, 0.3), true},
		{"stopped", reading(0.3, 0.99), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPriorityFake(t)
			p.addGoal(orgID, "steady", "normal")
			p.addHeld(orgID, "steady", "held-one", 1)
			p.addItem(orgID, "steady", "conflicted", 2, false)
			p.WorkItems["conflicted"]["status"], p.WorkItems["conflicted"]["rebaseState"] = "review", "requested"
			loop, logs, _ := p.pacedLoop(2, c.rl)
			var rebased []string
			var mu sync.Mutex
			loop.Rebase = func(_ context.Context, it gateway.WorkItem) {
				mu.Lock()
				rebased = append(rebased, it.Title)
				mu.Unlock()
			}
			loop.Tick(context.Background())
			loop.wg.Wait()
			evaluated := strings.Join(started(logs.String(), "evaluating item"), ",")
			claims := len(p.CallsNamed("claimWorkItemRebase"))
			if c.starts {
				if evaluated != "held-one" || claims != 1 || strings.Join(rebased, ",") != "conflicted" {
					t.Fatalf("paced: evaluated %q, rebase claims %d, rebased %v\n%s", evaluated, claims, rebased, logs)
				}
				if strings.Contains(logs.String(), "accounts: no account has headroom") {
					t.Fatalf("paced: logged waiting\n%s", logs)
				}
				return
			}
			if evaluated != "" || claims != 0 || len(rebased) != 0 {
				t.Fatalf("stopped: evaluated %q, rebase claims %d, rebased %v\n%s", evaluated, claims, rebased, logs)
			}
			if !strings.Contains(logs.String(), "accounts: no account has headroom for 1 evaluation(s), 1 rebase(s)") {
				t.Fatalf("stopped: the wait is not logged\n%s", logs)
			}
		})
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

// With no node.yaml accounts and two issued subscriptions known, an
// exhausted reading under `default` from an earlier session paces
// nothing: the node is judged on the two it runs on, and a normal item
// is claimed on one of them.
func TestLoopPacesOnIssuedAccountsNotDefault(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	loop, logs, _ := p.pacedLoop(2, reading(0.2, 0.97))
	loop.Known = issuedPair(t, "")
	loop.Limits.Observe("max20", reading(0.1, 0.2))
	loop.Limits.Observe("shhemail", reading(0.1, 0.2))
	var mu sync.Mutex
	var on []string
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) {
		a, _ := ChosenAccount(ctx)
		mu.Lock()
		on = append(on, a.Label)
		mu.Unlock()
	}

	if m := loop.pacing().mode; m != paceNormal {
		t.Fatalf("pacing = %v, want normal", m)
	}
	if claimed := p.claimedBy(loop); strings.Join(claimed, ",") != "normal-1" {
		t.Fatalf("claimed %v, want normal-1\n%s", claimed, logs)
	}
	if len(on) != 1 || (on[0] != "max20" && on[0] != "shhemail") {
		t.Fatalf("the item runs on %v, want an issued label", on)
	}
	if strings.Contains(logs.String(), "limits: stopped") {
		t.Fatalf("default's reading stopped the node:\n%s", logs)
	}
}

// The loop chooses between the issued accounts by their readings: the
// paced one is passed over for normal work, and with both stopped the
// tick's line names both.
func TestLoopChoosesAmongIssuedAccounts(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	loop, logs, _ := p.pacedLoop(2, nil)
	loop.Known = issuedPair(t, "")
	loop.Limits.Observe("max20", reading(0.85, 0.2))
	loop.Limits.Observe("shhemail", reading(0.1, 0.2))

	ctx, ok := loop.accountFor(context.Background(), "normal", true, "item")
	a, chosen := ChosenAccount(ctx)
	if !ok || !chosen || a.Label != "shhemail" {
		t.Fatalf("chose %+v (ok=%v, chosen=%v), want shhemail", a, ok, chosen)
	}

	stopped := reading(0.99, 0.2)
	stopped.ObservedAt = t0.Add(time.Minute)
	loop.Limits.Observe("max20", stopped)
	loop.Limits.Observe("shhemail", stopped)
	if claimed := p.claimedBy(loop); len(claimed) != 0 {
		t.Fatalf("both stopped, claimed %v", claimed)
	}
	const line = "accounts: no account has headroom for 1 item(s); judged max20, shhemail; left unclaimed until one has"
	if !strings.Contains(logs.String(), line) {
		t.Fatalf("want %q in:\n%s", line, logs)
	}
}

// A restart knows the labels an earlier store saved before any claim
// has run: its first tick schedules on them.
func TestLoopSchedulesOnKnownLabelsAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	issuedPair(t, path)
	known, err := OpenKnownAccounts(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addItem(orgID, "steady", "normal-1", 1, false)
	loop, _, _ := p.pacedLoop(2, reading(0.2, 0.97))
	loop.Known = known
	loop.Limits.Observe("max20", reading(0.97, 0.2))
	loop.Limits.Observe("shhemail", reading(0.1, 0.2))
	var on string
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) {
		a, _ := ChosenAccount(ctx)
		on = a.Label
	}
	if accounts, _ := loop.accounts(); labels(accounts) != "max20, shhemail" {
		t.Fatalf("accounts = %s", labels(accounts))
	}
	if claimed := p.claimedBy(loop); strings.Join(claimed, ",") != "normal-1" || on != "shhemail" {
		t.Fatalf("claimed %v on %q, want normal-1 on shhemail", claimed, on)
	}
}

// With no issued account known and none in node.yaml the loop is what
// it was: the one `default`, judged by the strictest reading.
func TestLoopWithoutIssuedAccountsJudgesDefault(t *testing.T) {
	for name, known := range map[string]*KnownAccounts{"nil": nil, "empty": {}} {
		p := newPriorityFake(t)
		loop, _, _ := p.pacedLoop(2, reading(0.2, 0.3))
		loop.Known = known
		loop.Limits.Observe("/home/mini/.claude", reading(0.2, 0.99))
		accounts, readings := loop.accounts()
		if labels(accounts) != DefaultAccount || len(readings) != 1 || readings[DefaultAccount].SevenDay.Utilization != 0.99 {
			t.Fatalf("%s: accounts %s, readings %+v", name, labels(accounts), readings)
		}
		if m := loop.pacing().mode; m != paceStopped {
			t.Fatalf("%s: pacing = %v, want stopped", name, m)
		}
	}
}

// The pacing line names the account that paces once it is not the
// lone `default`.
func TestPacedLineNamesTheIssuedAccount(t *testing.T) {
	p := newPriorityFake(t)
	loop, logs, _ := p.pacedLoop(2, nil)
	loop.Known = &KnownAccounts{}
	if err := loop.Known.Note(AccountConfig{Label: "max20", Kind: AccountSubscription, ID: "cred-1"}, t0); err != nil {
		t.Fatal(err)
	}
	loop.Limits.Observe("max20", reading(0.85, 0.2))
	loop.pacing()
	if !strings.Contains(logs.String(), "limits: paced until 2026-09-25T15:00:00Z: max20: five-hour window at 85% (pace 80%)") {
		t.Fatalf("the pacing line does not name max20:\n%s", logs)
	}
}
