package node

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// The replay of the loom goal, 2026-10-03 (ADR-023, a refused plan is
// released, not re-planned): the goal held one failed item, and every
// plan of it — the ask and the re-ask alike — recreated that item at
// its acceptance. The plan check refused each final answer with the
// same sentence, the node planned it again, six times in 30 minutes, up
// to $5 a plan, and the platform was never told. Replayed on today's
// planner, the second identical refusal releases the plan claim with
// the refusal's text and no third plan runs until the goal changes.

const (
	loomGoalID = "10101010-1010-4010-8010-101010101010"
	loomItem1  = "10101010-1011-4011-8011-101010101011"

	loomAcceptance = "loom serves its looms over gRPC with mTLS; `go test ./internal/server/...` passes."

	// both answers: one item at the failed item's acceptance
	loomReLandPlan = "```json\n{\"items\": [{\"seq\": 2, \"title\": \"Serve the looms over gRPC, again\", \"brief\": \"Again.\", \"acceptance\": \"" + loomAcceptance + "\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"

	// the check's sentence, each time
	loomRefusal = "plan would re-land failed item 1 (lineage 1) at the same acceptance; refused"
)

var loomUpdatedAt = time.Date(2026, 10, 3, 9, 12, 0, 0, time.UTC)

// seedLoom is the fake as the loom goal read that night: in planning,
// one failed item, the project's one repo acme/widgets.
func seedLoom(t *testing.T) *gatewaytest.Fake {
	t.Helper()
	f := seed(t)
	f.Goals[loomGoalID] = map[string]any{"id": loomGoalID, "orgId": orgID, "projectId": projID, "slug": "loom", "title": "Loom serves its looms",
		"statement": "Loom serves its looms over gRPC.", "acceptance": "go test ./... passes", "budgetCents": 20000, "spentCents": 0,
		"status": "planning", "updatedAt": loomUpdatedAt.Format(time.RFC3339)}
	f.WorkItems[loomItem1] = map[string]any{"id": loomItem1, "goalId": loomGoalID, "orgId": orgID, "seq": 1, "title": "Serve the looms over gRPC",
		"brief": "The server.", "acceptance": loomAcceptance, "repoId": repoID, "baseRef": "main",
		"status": "failed", "lastError": "evaluation failed 3 times", "dependsOn": []string{}}
	f.Projects[projID]["repoIds"] = []string{repoID}
	return f
}

// loomPlanner is a planner on root, as `mesh node start` builds one per
// plan, answering every session with the re-land.
func loomPlanner(gw *gateway.Client, h *fakeHarness, root string, logged *strings.Builder) (*Planner, *Config) {
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	return &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: &fakeWS{root: root}, Cfg: cfg, WorkRoot: root, Log: log.New(logged, "", 0)}, cfg
}

// requestPlan is planRequested's path for one goal: the row read, a
// released goal passed over, else the claim and the plan.
func requestPlan(t *testing.T, gw *gateway.Client, pl *Planner, cfg *Config) error {
	t.Helper()
	ctx := context.Background()
	g, err := gw.Goal(ctx, loomGoalID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pl.Released(*g); ok {
		return nil
	}
	if err := gw.ClaimPlan(ctx, g.ID, cfg.NodeID); err != nil {
		t.Fatal(err)
	}
	return pl.Plan(ctx, *g, scopesOf(cfg)[0])
}

func TestReplayLoomRefusedPlanIsReleasedNotReplanned(t *testing.T) {
	f := seedLoom(t)
	gw := gateway.New(f.URL(), "rs_test")
	h := &fakeHarness{text: loomReLandPlan}
	root := t.TempDir()
	var logged strings.Builder

	// six requested plans across a restart: three on each Planner
	pl, cfg := loomPlanner(gw, h, root, &logged)
	first := requestPlan(t, gw, pl, cfg)
	if pr, ok := PlanRefusedOf(first); !ok || pr.Released || pr.Reason != loomRefusal {
		t.Fatalf("first plan = %#v, want a *PlanRefused %q, not released", first, loomRefusal)
	}
	for range 2 {
		_ = requestPlan(t, gw, pl, cfg)
	}
	pl, cfg = loomPlanner(gw, h, root, &logged) // the node restarts
	for range 3 {
		_ = requestPlan(t, gw, pl, cfg)
	}

	if len(h.jobs) != 4 {
		t.Fatalf("plan sessions = %d, want 4 (two plans, an ask and a re-ask each)\n%s", len(h.jobs), logged.String())
	}
	if len(f.PlanReleases) != 1 || f.PlanReleases[0].Reason != loomRefusal || f.PlanReleases[0].GoalID != loomGoalID || f.PlanReleases[0].NodeID != cfg.NodeID {
		t.Fatalf("plan releases = %+v, want one with reason %q", f.PlanReleases, loomRefusal)
	}
	// no claimPlan after the release
	released := false
	claims := 0
	for _, c := range f.Calls {
		switch c.Name {
		case "releaseGoalPlan":
			released = true
		case "claimPlan":
			if released {
				t.Fatalf("claimPlan sent after the release: %+v", c)
			}
			claims++
		}
	}
	if claims != 2 {
		t.Fatalf("claimPlan calls = %d, want 2", claims)
	}
	if n := strings.Count(logged.String(), "released, not re-planned"); n != 1 {
		t.Fatalf("%d released lines, want 1:\n%s", n, logged.String())
	}
	if !strings.Contains(logged.String(), "goal loom: plan refused twice with the same reason; released, not re-planned: "+loomRefusal) {
		t.Fatalf("log lacks the release line:\n%s", logged.String())
	}
	if pl.attempts[loomGoalID] != 0 {
		t.Fatalf("attempts on the restarted planner = %d, want 0", pl.attempts[loomGoalID])
	}

	// a person changes the goal: its row's updatedAt moves on, and it is
	// planned again
	f.Goals[loomGoalID]["updatedAt"] = loomUpdatedAt.Add(time.Hour).Format(time.RFC3339)
	_ = requestPlan(t, gw, pl, cfg)
	if len(h.jobs) != 6 {
		t.Fatalf("plan sessions after the change = %d, want 6", len(h.jobs))
	}
	if len(f.CallsNamed("claimPlan")) != 3 {
		t.Fatalf("claimPlan calls after the change = %d, want 3", len(f.CallsNamed("claimPlan")))
	}
	if len(f.PlanReleases) != 1 {
		t.Fatalf("plan releases after one changed plan = %d, want still 1", len(f.PlanReleases))
	}
}

// Two refusals differing only in case, whitespace and a trailing full
// stop are the same refusal.
func TestNormaliseRefusal(t *testing.T) {
	a := normaliseRefusal("Plan would re-land failed item 1 (lineage 1)\n  at the same acceptance; REFUSED.")
	b := normaliseRefusal(" plan would re-land failed item 1 (lineage 1) at the same acceptance;  refused")
	if a != b {
		t.Fatalf("%q != %q", a, b)
	}
	if normaliseRefusal("plan would re-land failed item 1 (lineage 1)") == normaliseRefusal("plan would re-land failed item 2 (lineage 1)") {
		t.Fatal("refusals naming different items normalise the same")
	}
}

// A platform without releaseGoalPlan: the goal is still released on the
// node, said once.
func TestRefusedPlanReleasedOnAnOldPlatform(t *testing.T) {
	f := seedLoom(t)
	f.Unsupported["releaseGoalPlan"] = true
	gw := gateway.New(f.URL(), "rs_test")
	h := &fakeHarness{text: loomReLandPlan}
	var logged strings.Builder
	pl, cfg := loomPlanner(gw, h, t.TempDir(), &logged)
	for range 4 {
		_ = requestPlan(t, gw, pl, cfg)
	}
	if len(h.jobs) != 4 {
		t.Fatalf("plan sessions = %d, want 4", len(h.jobs))
	}
	g, _ := gw.Goal(context.Background(), loomGoalID)
	if reason, ok := pl.Released(*g); !ok || reason != loomRefusal {
		t.Fatalf("Released = %q, %v, want %q, true", reason, ok, loomRefusal)
	}
	if n := strings.Count(logged.String(), "platform has no releaseGoalPlan; held on the node"); n != 1 {
		t.Fatalf("%d fallback lines, want 1:\n%s", n, logged.String())
	}
	if len(f.PlanReleases) != 0 {
		t.Fatalf("plan releases = %+v, want none", f.PlanReleases)
	}
}

// planRequested on a released goal sends no claimPlan and holds it, kind
// plan with the refusal; a changed row clears the hold and plans it.
func TestPlanRequestedHoldsAReleasedGoal(t *testing.T) {
	f := seedLoom(t)
	gw := gateway.New(f.URL(), "rs_test")
	h := &fakeHarness{text: loomReLandPlan}
	var logged strings.Builder
	pl, cfg := loomPlanner(gw, h, t.TempDir(), &logged)
	for range 2 {
		_ = requestPlan(t, gw, pl, cfg)
	}
	claims := len(f.CallsNamed("claimPlan"))
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: pl.Log,
		Dispatch:     func(context.Context, gateway.WorkItem) {},
		Plan:         func(ctx context.Context, g gateway.Goal, sc Scope) { _ = pl.Plan(ctx, g, sc) },
		PlanReleased: pl.Released}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.planRequested(ctx)
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimPlan")); n != claims {
		t.Fatalf("claimPlan calls %d → %d, want none on a released goal", claims, n)
	}
	held := loop.HeldGoals()
	if len(held) != 1 || held[0].GoalID != loomGoalID || held[0].Kind != "plan" || held[0].Reason != loomRefusal || held[0].Slug != "loom" {
		t.Fatalf("held = %+v, want the loom goal held for plan with the refusal", held)
	}

	f.Goals[loomGoalID]["updatedAt"] = loomUpdatedAt.Add(time.Hour).Format(time.RFC3339)
	loop.planRequested(ctx)
	loop.wg.Wait()
	if held := loop.HeldGoals(); len(held) != 0 {
		t.Fatalf("held after the change = %+v, want none", held)
	}
	if n := len(f.CallsNamed("claimPlan")); n != claims+1 {
		t.Fatalf("claimPlan calls %d → %d, want one after the change", claims, n)
	}
	if !strings.Contains(logged.String(), "held: acme goal loom (plan) released") {
		t.Fatalf("log lacks the hold's release:\n%s", logged.String())
	}
}
