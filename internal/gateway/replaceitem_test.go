package gateway_test

import (
	"context"
	"errors"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// RequestPlanReplacing sends replaceItem in RequestPlanInput, beside by,
// to a gateway whose input has it — asked about once (ADR-021's
// amendment 2026-10-02).
func TestRequestPlanReplacingSendsReplaceItem(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Goals["goal-1"] = map[string]any{"id": "goal-1", "status": "running"}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	for range 2 {
		if err := gw.RequestPlanReplacing(ctx, "goal-1", "node-1", "item-1"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.ReadsNamed("__type RequestPlanInput")); n != 1 {
		t.Fatalf("RequestPlanInput asked %d times, want 1", n)
	}
	calls := f.CallsNamed("requestPlan")
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	in := calls[0].Input
	if in["replaceItem"] != "item-1" || in["by"] != "node-1" || in["aggregateId"] != "goal-1" {
		t.Fatalf("input = %+v", in)
	}
	if reqs := f.RequestPlans(); len(reqs) != 2 || reqs[0] != (gatewaytest.PlanRequest{GoalID: "goal-1", By: "node-1", ReplaceItem: "item-1"}) {
		t.Fatalf("RequestPlans = %+v", reqs)
	}
}

// A platform that refuses the narrow re-plan answers a refusal, returned
// as such; one whose RequestPlanInput lacks replaceItem is sent nothing.
func TestRequestPlanReplacingRefusedAndOlder(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Goals["goal-1"] = map[string]any{"id": "goal-1", "status": "running"}
	f.Refuse["requestPlan"] = "item 1 re-landed 3 times; a person decides now"
	gw := gateway.New(f.URL(), "rs_test")
	if err := gw.RequestPlanReplacing(context.Background(), "goal-1", "node-1", "item-1"); !gateway.IsRefusal(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}

	old := gatewaytest.New("rs_test")
	defer old.Close()
	old.NoReplaceItem = true
	old.Goals["goal-1"] = map[string]any{"id": "goal-1", "status": "running"}
	gw = gateway.New(old.URL(), "rs_test")
	if err := gw.RequestPlanReplacing(context.Background(), "goal-1", "node-1", "item-1"); !errors.Is(err, gateway.ErrNoReplaceItem) {
		t.Fatalf("err = %v, want ErrNoReplaceItem", err)
	}
	if reqs := old.RequestPlans(); len(reqs) != 0 {
		t.Fatalf("an older gateway was sent %+v", reqs)
	}
}
