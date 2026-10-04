package node

import (
	"context"
	"sync/atomic"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
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
