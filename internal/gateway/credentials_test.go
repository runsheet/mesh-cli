package gateway_test

import (
	"context"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/node"
)

// The real client is what the node reads credentials and records usage through.
var _ node.CredentialGateway = (*gateway.Client)(nil)

// The claim-time read sends the session and the node and decodes each
// credential whole.
func TestSessionCredentials(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.SessionCredentials[itemID] = []map[string]any{
		{"id": "c1", "label": "max-a", "kind": "subscription", "secret": "sk-ant-oat01-x"},
		{"id": "c2", "label": "key", "kind": "api", "secret": "sk-ant-api-y", "capDailyUsd": 50.0, "capMonthlyUsd": 1000.0, "spentDailyUsd": 12.5, "spentMonthlyUsd": 300.0},
	}
	gw := gateway.New(f.URL(), "rs_test")
	got, err := gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "build", ItemID: itemID, GoalID: "g"}, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	want := []gateway.Credential{
		{ID: "c1", Label: "max-a", Kind: "subscription", Secret: "sk-ant-oat01-x"},
		{ID: "c2", Label: "key", Kind: "api", Secret: "sk-ant-api-y", CapDailyUSD: 50, CapMonthlyUSD: 1000, SpentDailyUSD: 12.5, SpentMonthlyUSD: 300},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("credentials = %#v", got)
	}
	in := f.CallsNamed("sessionCredentials")[0].Input
	if in["workItemId"] != itemID || in["goalId"] != "g" || in["kind"] != "build" || in["nodeId"] != nodeID {
		t.Fatalf("input = %v", in)
	}
}

// A platform without the read or the ledger answers IsUnsupported: the
// node's cue to fall back, not to fail.
func TestCredentialsUnsupported(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Unsupported["sessionCredentials"] = true
	f.Unsupported["recordSessionUsage"] = true
	gw := gateway.New(f.URL(), "rs_test")
	if _, err := gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "plan", GoalID: "g"}, nodeID); !gateway.IsUnsupported(err) {
		t.Fatalf("SessionCredentials = %v, want unsupported", err)
	}
	if err := gw.RecordSessionUsage(context.Background(), gateway.UsageRecord{Kind: "plan"}); !gateway.IsUnsupported(err) {
		t.Fatalf("RecordSessionUsage = %v, want unsupported", err)
	}
}

// A usage record goes in as one input with its windows nested.
func TestRecordSessionUsage(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	rec := gateway.UsageRecord{Kind: "research", DirectionID: "d", CredentialLabel: "max-a", InputTokens: 10, OutputTokens: 2, CacheReadTokens: 5, CostUSD: 0.5,
		After: &gateway.UsageWindows{FiveHour: &gateway.LimitWindow{Utilization: 0.4, ResetsAt: at}}, StartedAt: at, EndedAt: at.Add(time.Minute)}
	if err := gw.RecordSessionUsage(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	in := f.CallsNamed("recordSessionUsage")[0].Input
	after, _ := in["after"].(map[string]any)
	five, _ := after["fiveHour"].(map[string]any)
	if in["directionId"] != "d" || in["inputTokens"] != 10.0 || in["cacheReadTokens"] != 5.0 || five["utilization"] != 0.4 || in["endedAt"] != "2026-09-25T10:01:00Z" || in["namespace"] != "runsheet" {
		t.Fatalf("input = %v", in)
	}
	if _, ok := in["before"]; ok {
		t.Errorf("a record with no before-reading sent one: %v", in["before"])
	}
}
