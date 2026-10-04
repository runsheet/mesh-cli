package node

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// node.yaml's `takes:` (ADR-025): a kind the node does not take is
// skipped before its claim path lists anything or asks the platform, so
// nothing is claimed and released.

// takesFake is a priorityFake offering one session of every kind: in
// acme a ready item, a goal in planning, a requested rebase and a live
// direction with room; in globex an item this node holds in evaluating
// — apart, since an evaluation reads its own goal's org for the
// priority, so acme's goals are read only by a build, plan, rebase or
// research path.
func takesFake(t *testing.T) *priorityFake {
	p := newPriorityFake(t)
	p.addGoal(orgID, "run", "normal")
	p.addPlanning(orgID, "plan", "normal")
	p.addItem(orgID, "run", "ready-1", 1, false)
	p.addItemRow(orgID, "run", "rebase-2", 2, map[string]any{"status": "review", "prUrl": "https://github.com/acme/app/pull/2", "rebaseState": "requested"})
	p.addGoal(org2, "held", "normal")
	p.addHeld(org2, "held", "held-3", 3)
	direction(p.Fake, dirID, projID, "nightly", "Tidy up.", 5)
	return p
}

// takesTick runs one tick of a capacity-2 loop over takesFake under
// takes and reports whether the loop's Evaluate func was started.
func takesTick(t *testing.T, p *priorityFake, takes Takes) bool {
	t.Helper()
	loop, _ := p.loop(2)
	loop.Cfg.Takes = takes
	var evaluated atomic.Bool
	loop.Evaluate = func(context.Context, gateway.WorkItem) { evaluated.Store(true) }
	loop.Rebase = func(context.Context, gateway.WorkItem) {}
	loop.Tick(context.Background())
	loop.wg.Wait()
	return evaluated.Load()
}

// readsWhere are the reads of name whose filter key is value.
func readsWhere(p *priorityFake, name, key, value string) int {
	n := 0
	for _, r := range p.ReadsNamed(name) {
		if r.Input[key] == value {
			n++
		}
	}
	return n
}

func TestTakesWithoutEvaluateNeverEvaluates(t *testing.T) {
	p := takesFake(t)
	evaluated := takesTick(t, p, Takes{{Kind: "build"}, {Kind: "rebase"}, {Kind: "plan"}, {Kind: "research"}})
	for _, name := range []string{"claimWorkItem", "claimPlan", "claimWorkItemRebase"} {
		if len(p.CallsNamed(name)) == 0 {
			t.Errorf("no %s call under takes: [build, rebase, plan, research]", name)
		}
	}
	if evaluated {
		t.Error("the Evaluate func started under a takes: without evaluate")
	}
	if n := readsWhere(p, "workItemIndexs", "status", "evaluating"); n != 0 {
		t.Errorf("%d evaluating listings read under a takes: without evaluate", n)
	}
}

func TestTakesEvaluateOnlyClaimsNothingElse(t *testing.T) {
	p := takesFake(t)
	evaluated := takesTick(t, p, Takes{{Kind: "evaluate"}})
	for _, name := range []string{"claimWorkItem", "claimPlan", "claimWorkItemRebase", "claimDirectionResearch"} {
		if n := len(p.CallsNamed(name)); n != 0 {
			t.Errorf("%d %s call(s) under takes: [evaluate]", n, name)
		}
	}
	if n := readsWhere(p, "workItemIndexs", "status", "ready"); n != 0 {
		t.Errorf("%d ready listings read under takes: [evaluate]", n)
	}
	if n := readsWhere(p, "workItemIndexs", "rebase_state", "requested"); n != 0 {
		t.Errorf("%d requested-rebase listings read under takes: [evaluate]", n)
	}
	if n := readsWhere(p, "goalIndexs", "org_id", orgID); n != 0 {
		t.Errorf("%d goal listings of acme read under takes: [evaluate]", n)
	}
	if n := len(p.ReadsNamed("directionIndexs")); n != 0 {
		t.Errorf("%d direction listings read under takes: [evaluate]", n)
	}
	if !evaluated {
		t.Error("the held item's Evaluate did not start under takes: [evaluate]")
	}
}

// Left out, takes: is every kind: the loop's accessor says so and every
// kind's claim path runs.
func TestTakesLeftOutTakesEveryKind(t *testing.T) {
	p := takesFake(t)
	evaluated := takesTick(t, p, nil)
	for _, name := range []string{"claimWorkItem", "claimPlan", "claimWorkItemRebase", "claimDirectionResearch"} {
		if len(p.CallsNamed(name)) == 0 {
			t.Errorf("no %s call with takes: left out", name)
		}
	}
	if !evaluated {
		t.Error("no evaluation started with takes: left out")
	}
	// the listings the takes: tests find absent are read by these keys
	for _, r := range []struct{ name, key, value string }{
		{"workItemIndexs", "status", "evaluating"}, {"workItemIndexs", "status", "ready"},
		{"workItemIndexs", "rebase_state", "requested"}, {"goalIndexs", "org_id", orgID},
	} {
		if readsWhere(p, r.name, r.key, r.value) == 0 {
			t.Errorf("no %s %s=%s read with takes: left out", r.name, r.key, r.value)
		}
	}
	loop, _ := p.loop(1)
	if got := loop.Takes().String(); got != "every kind" {
		t.Errorf("Takes() = %q, want every kind", got)
	}
}

// (a) capacity 4, takes: {evaluate: 2}, four items held here in
// evaluating: one tick starts exactly two evaluations, the next
// heartbeat's holding names the cap for the other two, and once one
// ends the next tick starts a third.
func TestTakesCapBoundsEvaluations(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "held", "normal")
	for i := 1; i <= 4; i++ {
		p.addHeld(orgID, "held", fmt.Sprintf("held-%d", i), i)
	}
	p.register()
	loop, logs := p.loop(4)
	loop.Cfg.Takes = Takes{{Kind: "evaluate", Cap: 2}}
	var started atomic.Int32
	release := make(chan struct{})
	loop.Evaluate = func(context.Context, gateway.WorkItem) {
		started.Add(1)
		<-release
	}
	defer func() { close(release); loop.wg.Wait() }()

	loop.Tick(context.Background())
	waitFor(t, func() bool { return started.Load() == 2 })
	if n := loop.runningOf(harness.KindEvaluate); n != 2 {
		t.Fatalf("%d evaluations running after one tick, want 2", n)
	}
	if !strings.Contains(logs.String(), "takes: evaluate at its cap of 2; 2 waiting") {
		t.Errorf("no cap line in the log:\n%s", logs)
	}

	// the next tick's heartbeat carries this one's holding; still at
	// the cap, it starts nothing
	loop.Tick(context.Background())
	u := unclaimedOf(t, p.lastHolding())["held/evaluation"]
	if u == nil || !strings.Contains(fmt.Sprint(u["reason"]), "takes cap of 2") {
		t.Fatalf("holding.unclaimed held/evaluation = %#v, want the takes cap", u)
	}
	if u["count"] != float64(2) {
		t.Errorf("held/evaluation count = %v, want 2", u["count"])
	}
	if n := loop.runningOf(harness.KindEvaluate); n != 2 {
		t.Fatalf("%d evaluations running after a second tick at the cap, want 2", n)
	}

	release <- struct{}{}
	waitFor(t, func() bool { return loop.runningOf(harness.KindEvaluate) == 1 })
	loop.Tick(context.Background())
	waitFor(t, func() bool { return started.Load() == 3 })
	if n := loop.runningOf(harness.KindEvaluate); n != 2 {
		t.Errorf("%d evaluations running after one ended, want 2", n)
	}
	if n := started.Load(); n != 3 {
		t.Errorf("%d evaluations started in all, want 3", n)
	}
}

// (b) capacity 3, takes: {build: 1}, three ready items: one tick
// claims one.
func TestTakesCapBoundsBuilds(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "run", "normal")
	for i := 1; i <= 3; i++ {
		p.addItem(orgID, "run", fmt.Sprintf("ready-%d", i), i, false)
	}
	loop, _ := p.loop(3)
	loop.Cfg.Takes = Takes{{Kind: "build", Cap: 1}}
	loop.Tick(context.Background())
	loop.wg.Wait()
	if n := len(p.CallsNamed("claimWorkItem")); n != 1 {
		t.Errorf("%d claimWorkItem calls under takes: {build: 1}, want 1", n)
	}
	if u := loop.Holding(); u == nil || !holdsFor(u.Unclaimed, "run", "item", "takes cap of 1 build(s)") {
		t.Errorf("holding = %+v, want the build cap on run", u)
	}
}

// (c) a cap on a kind capacity does not bound: takes: {plan: 1} with
// two goals in planning claims one plan.
func TestTakesCapBoundsPlans(t *testing.T) {
	p := newPriorityFake(t)
	p.addPlanning(orgID, "plan-a", "normal")
	p.addPlanning(orgID, "plan-b", "normal")
	loop, _ := p.loop(2)
	loop.Cfg.Takes = Takes{{Kind: "plan", Cap: 1}}
	release := make(chan struct{})
	loop.Plan = func(context.Context, gateway.Goal, Scope) { <-release }
	loop.Tick(context.Background())
	close(release)
	loop.wg.Wait()
	if n := len(p.CallsNamed("claimPlan")); n != 1 {
		t.Errorf("%d claimPlan calls under takes: {plan: 1}, want 1", n)
	}
}

// holdsFor says whether unclaimed names goal and kind with reason.
func holdsFor(unclaimed []gateway.HoldingUnclaimed, goal, kind, reason string) bool {
	for _, u := range unclaimed {
		if u.GoalID == goal && u.Kind == kind && strings.Contains(u.Reason, reason) {
			return true
		}
	}
	return false
}
