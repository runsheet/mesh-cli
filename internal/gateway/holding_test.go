package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
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

// A gateway whose holding.unclaimed element type lacks `until` gets a
// heartbeat whose unclaimed entry carries no `until` key, even though
// Holding.Unclaimed[0].Until is set: the platform predates
// no-headroom-is-a-hold-not-a-claim-loop item 4.
func TestHeartbeatOmitsUnclaimedUntilTheSchemaLacks(t *testing.T) {
	f, gw := registeredFake(t)
	f.NoUnclaimedUntil = true
	h := holding()
	h.Unclaimed[0].Until = time.Date(2026, 10, 3, 3, 0, 0, 0, time.UTC)
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 1, gateway.HeartbeatExtras{Holding: h}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	entry := unclaimedEntry(t, hb[0].Input)
	if _, ok := entry["until"]; ok {
		t.Fatalf("sent until the schema lacks: %#v", entry)
	}

	// the platform deploys the field; the next probe (after
	// ScalingProbeEvery) sees it and sends it
	f.NoUnclaimedUntil = false
	gw.Now = func() time.Time { return time.Now().Add(gateway.ScalingProbeEvery) }
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 1, gateway.HeartbeatExtras{Holding: h}); err != nil {
		t.Fatal(err)
	}
	hb = f.CallsNamed("heartbeatNode")
	entry = unclaimedEntry(t, hb[len(hb)-1].Input)
	if entry["until"] != "2026-10-03T03:00:00Z" {
		t.Fatalf("until = %#v, want sent once the schema has it", entry["until"])
	}
}

// unclaimedEntry is a heartbeat input's lone holding.unclaimed entry.
func unclaimedEntry(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	h, ok := in["holding"].(map[string]any)
	if !ok {
		t.Fatalf("holding = %#v", in["holding"])
	}
	unclaimed, ok := h["unclaimed"].([]any)
	if !ok || len(unclaimed) != 1 {
		t.Fatalf("holding.unclaimed = %#v", h["unclaimed"])
	}
	entry, ok := unclaimed[0].(map[string]any)
	if !ok {
		t.Fatalf("unclaimed[0] = %#v", unclaimed[0])
	}
	return entry
}

// The heartbeat carries holding.orgs, each org's floor, held and ready,
// only while the gateway's holding input takes `orgs` (ADR-023,
// Reporting); and heartbeatNode.json's input holding decodes into
// Holding with its orgs, no key the client does not know.
func TestHeartbeatHoldingOrgs(t *testing.T) {
	f, gw := registeredFake(t)
	h := holding()
	h.Orgs = []gateway.HoldingOrg{
		{Org: "runsheet", OrgID: "org-r", Floor: 2, Held: 1, Ready: 3},
		{Org: "wardroomid", OrgID: "org-w", Floor: 2, Held: 2, Ready: 0},
	}
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 1, gateway.HeartbeatExtras{Holding: h}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	sent, _ := hb[len(hb)-1].Input["holding"].(map[string]any)
	orgs, ok := sent["orgs"].([]any)
	if !ok || len(orgs) != 2 {
		t.Fatalf("holding.orgs = %#v", sent["orgs"])
	}
	if o := orgs[1].(map[string]any); o["org"] != "wardroomid" || o["orgId"] != "org-w" || o["floor"] != 2.0 || o["held"] != 2.0 || o["ready"] != 0.0 {
		t.Fatalf("orgs[1] = %#v", o)
	}

	// a platform whose holding input lacks `orgs`: the next probe sees
	// it gone and the field is dropped, the rest of holding sent
	f.NoHoldingOrgs = true
	gw.Now = func() time.Time { return time.Now().Add(gateway.ScalingProbeEvery) }
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 1, gateway.HeartbeatExtras{Holding: h}); err != nil {
		t.Fatal(err)
	}
	hb = f.CallsNamed("heartbeatNode")
	sent, _ = hb[len(hb)-1].Input["holding"].(map[string]any)
	if _, ok := sent["orgs"]; ok || sent["mode"] != "paced" {
		t.Fatalf("holding without orgs = %#v", sent)
	}

	data, err := os.ReadFile("../../specs/contracts/heartbeatNode.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Description string `json:"description"`
		Input       struct {
			Holding json.RawMessage `json:"holding"`
		} `json:"input"`
	}
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(fx.Input.Holding))
	dec.DisallowUnknownFields()
	var got gateway.Holding
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("fixture holding: %v", err)
	}
	want := gateway.HoldingOrg{Org: "runsheet", OrgID: "0f3b8a1e-2c4d-4e6f-8a9b-0c1d2e3f4a5b", Floor: 2, Held: 1, Ready: 3}
	if len(got.Orgs) != 2 || got.Orgs[0] != want {
		t.Fatalf("fixture holding.orgs = %+v", got.Orgs)
	}
	if !strings.Contains(fx.Description, "holding.orgs") {
		t.Fatal("fixture description does not name holding.orgs")
	}
	if want := []string{"build", "rebase", "plan", "research"}; !reflect.DeepEqual(got.Kinds, want) {
		t.Fatalf("fixture holding.kinds = %q, want %q", got.Kinds, want)
	}
	if !strings.HasPrefix(got.Summary, "takes build, rebase, plan, research; ") {
		t.Fatalf("fixture holding.summary = %q", got.Summary)
	}
	if !strings.Contains(fx.Description, "holding.kinds") {
		t.Fatal("fixture description does not name holding.kinds")
	}
}
