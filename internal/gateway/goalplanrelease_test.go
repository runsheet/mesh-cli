package gateway_test

import (
	"context"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const refusalText = "plan names repo acme/other outside the goal's acme/widgets; a different goal; refused"

// ReleaseGoalPlan sends releaseGoalPlan with nodeId and reason, and the
// fake records it on PlanReleases and clears the claim (goal
// a-refused-plan-is-released-not-replanned).
func TestReleaseGoalPlan(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	goalID := "g1"
	f.Goals[goalID] = map[string]any{"id": goalID, "status": "planning", "plannerNodeId": nodeID, "planClaimedAt": "2026-10-03T04:00:00Z"}
	gw := gateway.New(f.URL(), "rs_test")

	if err := gw.ReleaseGoalPlan(context.Background(), goalID, nodeID, refusalText); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("releaseGoalPlan")
	if len(calls) != 1 || calls[0].Input["aggregateId"] != goalID ||
		calls[0].Input["nodeId"] != nodeID || calls[0].Input["reason"] != refusalText {
		t.Fatalf("calls = %+v", calls)
	}
	if len(f.PlanReleases) != 1 || f.PlanReleases[0] != (gatewaytest.PlanRelease{GoalID: goalID, NodeID: nodeID, Reason: refusalText}) {
		t.Fatalf("PlanReleases = %+v", f.PlanReleases)
	}
	row := f.Goal(goalID)
	if row["plannerNodeId"] != nil || row["planClaimedAt"] != nil {
		t.Fatalf("claim not cleared: %+v", row)
	}
}

// A platform without releaseGoalPlan answers IsUnsupported, the way
// ReleasePlan does.
func TestReleaseGoalPlanOldPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	goalID := "g1"
	f.Goals[goalID] = map[string]any{"id": goalID, "status": "planning", "plannerNodeId": nodeID}
	f.Unsupported["releaseGoalPlan"] = true
	gw := gateway.New(f.URL(), "rs_test")

	err := gw.ReleaseGoalPlan(context.Background(), goalID, nodeID, refusalText)
	if !gateway.IsUnsupported(err) {
		t.Fatalf("err = %v, want unsupported", err)
	}
}

// Goal.UpdatedAt reads a goal row's updatedAt key; an older platform
// refusing the field still answers the rest of the read, with a zero
// UpdatedAt.
func TestGoalReadsUpdatedAt(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Goals["g1"] = map[string]any{"id": "g1", "status": "planning", "updatedAt": "2026-10-03T04:27:21Z"}
	gw := gateway.New(f.URL(), "rs_test")

	g, err := gw.Goal(context.Background(), "g1")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 3, 4, 27, 21, 0, time.UTC)
	if !g.UpdatedAt.Equal(want) {
		t.Fatalf("UpdatedAt = %v, want %v", g.UpdatedAt, want)
	}
}

func TestGoalWithoutUpdatedAtOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields["updatedAt"] = true
	f.Goals["g1"] = map[string]any{"id": "g1", "status": "planning"}
	gw := gateway.New(f.URL(), "rs_test")

	for i := 0; i < 2; i++ {
		g, err := gw.Goal(context.Background(), "g1")
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if g.ID != "g1" || !g.UpdatedAt.IsZero() {
			t.Fatalf("read %d: %+v", i, g)
		}
	}
}

// Fake.ClaimRefusals makes claimWorkItem answer the configured message
// as the gateway error, the item left ready and still listed by the
// ready read (goal a-refused-plan-is-released-not-replanned, a claim
// refused for budget, pause or approval).
func TestClaimWorkItemRefusedByClaimRefusals(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.WorkItems[itemID] = map[string]any{"id": itemID, "orgId": orgID, "seq": 1, "status": "ready"}
	const msg = "org's account has paused spending"
	f.ClaimRefusals = map[string]string{itemID: msg}
	gw := gateway.New(f.URL(), "rs_test")

	err := gw.ClaimWorkItem(context.Background(), itemID, nodeID, "claude")
	if err == nil || err.Error() != msg {
		t.Fatalf("err = %v, want %q", err, msg)
	}
	it, err := gw.WorkItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "ready" {
		t.Fatalf("status = %q, want ready", it.Status)
	}
	items, err := gw.ReadyWorkItems(context.Background(), orgID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range items {
		if w.ID == itemID {
			found = true
		}
	}
	if !found {
		t.Fatalf("items = %+v, want %s listed", items, itemID)
	}
}
