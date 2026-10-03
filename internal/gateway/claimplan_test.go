package gateway_test

import (
	"context"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// The fake's claimPlan is runsheet goal.go's: a fresh claim stamps
// planClaimedAt; the holder may claim again at any time and keeps it;
// another node is refused while PlanClaimTTL has not passed since it,
// and takes the claim, freshly stamped, once it has.
func TestFakeClaimPlanTTL(t *testing.T) {
	const other = "33333333-3333-4333-8333-333333333333"
	f := gatewaytest.New("rs_test")
	defer f.Close()
	if f.PlanClaimTTLInForce() != 30*time.Minute {
		t.Fatalf("default TTL %s", f.PlanClaimTTLInForce())
	}
	now := time.Date(2026, 10, 3, 3, 30, 35, 0, time.UTC)
	f.Now = func() time.Time { return now }
	f.PlanClaimTTL = 10 * time.Minute
	goalID := planGoalID()
	f.Goals[goalID] = map[string]any{"id": goalID, "projectId": projID, "status": "planning"}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	if err := gw.ClaimPlan(ctx, goalID, nodeID); err != nil {
		t.Fatal(err)
	}
	claimedAt := "2026-10-03T03:30:35Z"
	if g := f.Goal(goalID); g["plannerNodeId"] != nodeID || g["planClaimedAt"] != claimedAt {
		t.Fatalf("fresh claim: %+v", g)
	}
	// the holder re-claims inside the TTL and keeps planClaimedAt
	now = now.Add(5 * time.Minute)
	if err := gw.ClaimPlan(ctx, goalID, nodeID); err != nil {
		t.Fatalf("same-node re-claim: %v", err)
	}
	if g := f.Goal(goalID); g["planClaimedAt"] != claimedAt {
		t.Fatalf("re-claim moved planClaimedAt: %+v", g)
	}
	// another node inside the TTL is refused
	now = now.Add(4*time.Minute + 59*time.Second)
	if err := gw.ClaimPlan(ctx, goalID, other); err == nil {
		t.Fatal("another node took the claim inside the TTL")
	}
	if g := f.Goal(goalID); g["plannerNodeId"] != nodeID || g["planClaimedAt"] != claimedAt {
		t.Fatalf("refused claim changed the goal: %+v", g)
	}
	// the holder past the TTL still re-claims, planClaimedAt kept
	now = now.Add(time.Hour)
	if err := gw.ClaimPlan(ctx, goalID, nodeID); err != nil {
		t.Fatalf("same-node re-claim past the TTL: %v", err)
	}
	if g := f.Goal(goalID); g["planClaimedAt"] != claimedAt {
		t.Fatalf("re-claim moved planClaimedAt: %+v", g)
	}
	// past the TTL another node takes it, freshly stamped
	if err := gw.ClaimPlan(ctx, goalID, other); err != nil {
		t.Fatalf("another node past the TTL: %v", err)
	}
	if g := f.Goal(goalID); g["plannerNodeId"] != other || g["planClaimedAt"] != now.UTC().Format(time.RFC3339) {
		t.Fatalf("expired claim taken: %+v", g)
	}
	// a claim with no planClaimedAt held by another is refused
	f.Goals[goalID]["planClaimedAt"] = nil
	if err := gw.ClaimPlan(ctx, goalID, nodeID); err == nil {
		t.Fatal("a held claim with no planClaimedAt was taken")
	}
}
