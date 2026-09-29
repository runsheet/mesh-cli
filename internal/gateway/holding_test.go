package gateway_test

import (
	"context"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// holding is a paced-subscription, exhausted-api-account holding
// report with one item left unclaimed.
func holding() *gateway.Holding {
	until := time.Date(2026, 10, 3, 3, 0, 0, 0, time.UTC)
	return &gateway.Holding{
		Mode: "paced", Summary: "paced: urgent and high only, 3 item(s), no research", Until: until,
		Accounts: []gateway.HoldingAccount{
			{Account: "max20", Kind: "subscription", Status: "paced", ResetsAt: until,
				Takes: gateway.HoldingTakes{Priorities: []string{"urgent", "high"}, MaxItems: 3}},
		},
		Unclaimed: []gateway.HoldingUnclaimed{
			{GoalID: "goal-1", Kind: "item", Count: 1, Reason: "no account has headroom"},
		},
	}
}

// A heartbeat with Holding set sends `holding`, with accounts and
// unclaimed always as lists.
func TestHeartbeatCarriesHolding(t *testing.T) {
	f, gw := registeredFake(t)
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 2, gateway.HeartbeatExtras{Holding: holding()}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	h, ok := hb[0].Input["holding"].(map[string]any)
	if !ok {
		t.Fatalf("holding = %#v", hb[0].Input["holding"])
	}
	if h["mode"] != "paced" || h["summary"] != "paced: urgent and high only, 3 item(s), no research" {
		t.Fatalf("holding = %#v", h)
	}
	accounts, ok := h["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("holding.accounts = %#v", h["accounts"])
	}
	unclaimed, ok := h["unclaimed"].([]any)
	if !ok || len(unclaimed) != 1 {
		t.Fatalf("holding.unclaimed = %#v", h["unclaimed"])
	}
}

// A heartbeat with Holding set but accounts and unclaimed both empty
// still sends [] for each, not null: the scaling precedent, PR #255.
func TestHeartbeatHoldingAccountsAndUnclaimedAreAlwaysLists(t *testing.T) {
	f, gw := registeredFake(t)
	x := gateway.HeartbeatExtras{Holding: &gateway.Holding{Mode: "normal", Summary: "normal: up to 4 item(s)"}}
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 1, x); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	h, _ := hb[0].Input["holding"].(map[string]any)
	accounts, ok := h["accounts"].([]any)
	if !ok || accounts == nil || len(accounts) != 0 {
		t.Fatalf("holding.accounts = %#v, want an empty list", h["accounts"])
	}
	unclaimed, ok := h["unclaimed"].([]any)
	if !ok || unclaimed == nil || len(unclaimed) != 0 {
		t.Fatalf("holding.unclaimed = %#v, want an empty list", h["unclaimed"])
	}
}

// A heartbeat with no Holding set sends no `holding` key.
func TestHeartbeatWithoutHoldingOmitsIt(t *testing.T) {
	f, gw := registeredFake(t)
	ctx := context.Background()
	for _, x := range [][]gateway.HeartbeatExtras{nil, {{}}, {{Draining: true}}} {
		if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 0, x...); err != nil {
			t.Fatal(err)
		}
	}
	for i, hb := range f.CallsNamed("heartbeatNode") {
		if v, ok := hb.Input["holding"]; ok {
			t.Fatalf("heartbeat %d sent holding = %#v", i, v)
		}
	}
}

// A gateway whose schema lacks `holding` gets a heartbeat without it,
// even when Holding is set: the platform predates
// node-says-why-work-waits.
func TestHeartbeatOmitsHoldingTheSchemaLacks(t *testing.T) {
	f, gw := registeredFake(t)
	f.UnknownFields["holding"] = true
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 1, gateway.HeartbeatExtras{Holding: holding()}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	if v, ok := hb[0].Input["holding"]; ok {
		t.Fatalf("sent holding the schema lacks: %#v", v)
	}

	// the platform deploys the field; the next probe (after
	// ScalingProbeEvery) sees it
	delete(f.UnknownFields, "holding")
	gw.Now = func() time.Time { return time.Now().Add(gateway.ScalingProbeEvery) }
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 1, gateway.HeartbeatExtras{Holding: holding()}); err != nil {
		t.Fatal(err)
	}
	hb = f.CallsNamed("heartbeatNode")
	if hb[len(hb)-1].Input["holding"] == nil {
		t.Fatalf("holding not sent once the schema has it: %#v", hb[len(hb)-1].Input)
	}
}
