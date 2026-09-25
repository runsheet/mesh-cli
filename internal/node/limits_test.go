package node

import (
	"context"
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
	if err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node"}); err != nil {
		t.Fatal(err)
	}
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		on(harness.Progress{SessionID: "s-1"})
		on(harness.Progress{SessionID: "s-1", RateLimit: reading(0.42, 0.17)})
		return harness.Result{Success: true, Text: "Done.", CostUSD: 0.5, SessionID: "s-1"}, nil
	})
	ws := &fakeWS{root: t.TempDir()}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Harness: "claude-code", Limits: &Limits{}, Now: func() time.Time { return t0 }}
	runner := &Runner{GW: gw, Harness: ObserveLimits(h, loop.Limits), WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour}
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) }

	loop.Tick(context.Background())
	loop.wg.Wait()
	if hb := f.CallsNamed("heartbeatNode"); len(hb) != 1 || hb[0].Input["limits"] != nil {
		t.Fatalf("no session has run yet, no limits to send: %+v", hb)
	}
	got := loop.Limits.Readings()[DefaultAccount]
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
	if l["account"] != DefaultAccount || l["status"] != "allowed" || five["utilization"] != 0.42 || seven["utilization"] != 0.17 ||
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
		{"five-hour at 95%", *reading(0.95, 0.5), LimitsConfig{}, t0, paceStopped, fiveOver},
		{"seven-day at 96%", *reading(0.85, 0.96), LimitsConfig{}, t0, paceStopped, weekOver},
		{"allowed_warning", status("allowed_warning"), LimitsConfig{}, t0, paceStopped, t0.Add(30 * time.Minute)},
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
// most 2; only the urgent/high goal is planned; no research.
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
	if strings.Join(claimed, ",") != "urgent-1,urgent-2" {
		t.Fatalf("paced claimed %v, want urgent-1,urgent-2", claimed)
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
		"seven-day 96%":   func() *harness.RateLimit { return reading(0.3, 0.96) },
		"allowed_warning": func() *harness.RateLimit { r := reading(0.3, 0.3); r.Status = "allowed_warning"; return r },
		"rejected":        func() *harness.RateLimit { r := reading(0.3, 0.3); r.Status = "rejected"; return r },
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
	for _, want := range []string{"limits: paced until", "limits: pace five-hour 50% → 80%, stop 90% → 95%", "limits: back to normal (was paced)"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, logs)
		}
	}
	if th := (LimitsConfig{}).Thresholds(); th.PaceFiveHour != DefaultPaceFiveHour || th.Stop != DefaultStop {
		t.Fatalf("defaults = %+v", th)
	}
}

// Ticks in one mode log its crossing once: into paced, into stopped,
// back to normal — each a single line however many ticks it lasts.
func TestLoopLogsEachCrossingOnce(t *testing.T) {
	p := newPriorityFake(t)
	loop, logs, c := p.pacedLoop(2, reading(0.85, 0.1))
	for range 3 {
		loop.Tick(context.Background())
	}
	stop := reading(0.97, 0.1)
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
