package node

import (
	"context"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/jobresult"
	"projectmesh.io/mesh-cli/internal/mcptool"
)

// The loop of 2026-10-26 on dl360: a goal set in a project whose repos
// do not hold its work. Its planner gave the plan up, then answered
// `split` with a draft naming a repo outside the project, then gave up
// again — never the same refusal twice running, and a give-up was no
// refusal at all — so a fresh Planner per job planned it every minute:
// 335 sessions overnight.

// foreignSplit is a `split` answer whose draft names a repo the
// project does not enrol.
var foreignSplit = map[string]any{"plan": map[string]any{"split": []any{map[string]any{
	"slug": "loom-node-git-config", "title": "The node runs no session's git config",
	"statement": "The node ignores a session's git config.", "acceptance": "`go test ./...` passes.",
	"repo": "runsheet/mesh-cli",
}}}}

// alternatingPlanner answers its sessions in turn: give up, a foreign
// split, give up, a foreign split…
func alternatingPlanner() *fakeHarness {
	var mu sync.Mutex
	run := 0
	h := &fakeHarness{}
	h.onRun = func(job harness.Job) {
		mu.Lock()
		n := run
		run++
		mu.Unlock()
		if n%2 == 0 {
			tool, _ := mcptool.Find(job.NodeTools, jobresult.FailTool)
			_, _ = tool.Call(context.Background(), map[string]any{"error": "This goal cannot be planned in project loom: its work is in runsheet/mesh-cli"})
			return
		}
		tool, _ := mcptool.Find(job.NodeTools, jobresult.ResultTool)
		_, _ = tool.Call(context.Background(), foreignSplit)
	}
	return h
}

// Refusals that never repeat word for word still release the claim:
// MaxPlanRefusals of them running, and the goal is not planned again by
// the next job's fresh Planner until its row changes.
func TestPlanAlternatingRefusalsAreReleased(t *testing.T) {
	f := seedLoom(t)
	gw := gateway.New(f.URL(), "rs_test")
	h := alternatingPlanner()
	root := t.TempDir()
	var logged strings.Builder

	for i := range 8 { // `mesh node start` builds a Planner per plan
		pl, cfg := loomPlanner(gw, h, root, &logged)
		err := requestPlan(t, gw, pl, cfg)
		if i == 0 {
			if pr, ok := PlanRefusedOf(err); !ok || pr.Released || !strings.Contains(pr.Reason, "the session gave the plan up") {
				t.Fatalf("first plan = %#v, want a give-up as a *PlanRefused, not released", err)
			}
		}
		if i == 1 {
			if pr, ok := PlanRefusedOf(err); !ok || pr.Released || !strings.Contains(pr.Reason, `names repo "runsheet/mesh-cli", not one of the project's`) {
				t.Fatalf("second plan = %#v, want the split draft's refusal, not released", err)
			}
		}
	}
	if len(h.jobs) != MaxPlanRefusals {
		t.Fatalf("plan sessions = %d, want %d\n%s", len(h.jobs), MaxPlanRefusals, logged.String())
	}
	if len(f.PlanReleases) != 1 {
		t.Fatalf("plan releases = %+v, want one", f.PlanReleases)
	}
	if !strings.Contains(logged.String(), "goal loom: plan refused 3 times running; released, not re-planned: ") {
		t.Fatalf("log lacks the release line:\n%s", logged.String())
	}
	if len(f.CallsNamed("planGoal")) != 0 || len(f.CallsNamed("proposeGoalSplit")) != 0 {
		t.Fatal("a plan or a split was handed in")
	}
}

// A plan handed in ends the streak: refusals either side of it are not
// counted together.
func TestPlanHandedInEndsTheRefusalStreak(t *testing.T) {
	pl := &Planner{WorkRoot: t.TempDir()}
	goal := gateway.Goal{ID: loomGoalID, Slug: "loom"}
	pl.mu.Lock()
	pl.loadRefusals()
	pl.lastRefusal[goal.ID] = "the session gave the plan up: x"
	pl.streak[goal.ID] = MaxPlanRefusals - 1
	pl.saveRefusals()
	pl.mu.Unlock()

	pl.handedIn(goal)

	fresh := &Planner{WorkRoot: pl.WorkRoot}
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	fresh.loadRefusals()
	if n := fresh.streak[goal.ID]; n != 0 {
		t.Fatalf("streak after a hand-in = %d, want 0", n)
	}
	if _, ok := fresh.lastRefusal[goal.ID]; ok {
		t.Fatal("last refusal kept after a hand-in")
	}
}
