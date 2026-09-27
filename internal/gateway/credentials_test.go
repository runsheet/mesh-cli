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

// anthropicSubscription and anthropicKey are itemCredentials rows as
// runsheet serves them (ADR 019): a Max subscription and an api key.
func anthropicSubscription(id, label, secret string) map[string]any {
	return map[string]any{"id": id, "label": label, "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"},
		"plan": "max-20x", "windowTargets": []any{}, "dailyCap": 0.0, "monthlyCap": 0.0, "capUnit": "dollars", "secretVersion": 1.0, "secret": secret}
}

func anthropicKey(id, label, secret string, daily, monthly float64) map[string]any {
	return map[string]any{"id": id, "label": label, "vendor": "anthropic", "kind": "api_key", "serves": []any{"claude-code"},
		"dailyCap": daily, "monthlyCap": monthly, "capUnit": "dollars", "secretVersion": 2.0, "secret": secret}
}

// The claim-time read of an item is itemCredentials(itemId): each
// Anthropic credential serving claude-code becomes a session account —
// a subscription (or an oauth token) as `subscription`, an api key as
// `api` with its dollar caps; a credential of another vendor is a tool's
// account and is not one; a key capped in calls has the node's default
// caps.
func TestSessionCredentials(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.SessionCredentials[itemID] = []map[string]any{
		anthropicSubscription("c1", "max-a", "sk-ant-oat01-x"),
		anthropicKey("c2", "key", "sk-ant-api-y", 50, 1000),
		{"id": "c3", "label": "oauth", "vendor": "anthropic", "kind": "oauth_token", "serves": []any{}, "secret": "sk-ant-oat01-z"},
		{"id": "c4", "label": "pplx", "vendor": "perplexity", "kind": "api_key", "serves": []any{"perplexity"}, "secret": "pplx-nope", "capUnit": "dollars", "dailyCap": 5.0},
		{"id": "c5", "label": "codex-only", "vendor": "anthropic", "kind": "subscription", "serves": []any{"codex"}, "secret": "sk-ant-oat01-codex"},
		{"id": "c6", "label": "by-calls", "vendor": "anthropic", "kind": "api_key", "serves": []any{"claude-code"}, "capUnit": "calls", "dailyCap": 100.0, "monthlyCap": 3000.0, "secret": "sk-ant-api-calls"},
	}
	gw := gateway.New(f.URL(), "rs_test")
	got, err := gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "build", ItemID: itemID, GoalID: "g"}, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	want := []gateway.Credential{
		{ID: "c1", Label: "max-a", Kind: "subscription", Secret: "sk-ant-oat01-x"},
		{ID: "c2", Label: "key", Kind: "api", Secret: "sk-ant-api-y", CapDailyUSD: 50, CapMonthlyUSD: 1000},
		{ID: "c3", Label: "oauth", Kind: "subscription", Secret: "sk-ant-oat01-z"},
		{ID: "c6", Label: "by-calls", Kind: "api", Secret: "sk-ant-api-calls"},
	}
	if len(got) != len(want) {
		t.Fatalf("credentials = %#v, want %d", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("credential %d = %#v (secret %q), want %#v", i, got[i], got[i].Secret, want[i])
		}
	}
	reads := f.CallsNamed("itemCredentials")
	if len(reads) != 1 || reads[0].Input["itemId"] != itemID {
		t.Fatalf("itemCredentials calls = %v", reads)
	}
	if len(f.CallsNamed("sessionCredentials")) != 0 {
		t.Fatal("the node still asks for sessionCredentials, which no platform serves")
	}
}

// A plan reads planCredentials(goalId) and research
// researchCredentials(directionId): each its own field, keyed on what the
// session works on.
func TestPlanAndResearchReadTheirOwnFields(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.SessionCredentials["g"] = []map[string]any{anthropicSubscription("c1", "max-a", "s1")}
	f.SessionCredentials["d"] = []map[string]any{anthropicKey("c2", "key", "s2", 20, 400)}
	gw := gateway.New(f.URL(), "rs_test")
	got, err := gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "plan", GoalID: "g"}, nodeID)
	if err != nil || len(got) != 1 || got[0].Label != "max-a" {
		t.Fatalf("plan: %v, %v", got, err)
	}
	if reads := f.CallsNamed("planCredentials"); len(reads) != 1 || reads[0].Input["goalId"] != "g" {
		t.Fatalf("planCredentials calls = %v", reads)
	}
	got, err = gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "research", DirectionID: "d"}, nodeID)
	if err != nil || len(got) != 1 || got[0].Kind != "api" || got[0].CapDailyUSD != 20 {
		t.Fatalf("research: %v, %v", got, err)
	}
	if reads := f.CallsNamed("researchCredentials"); len(reads) != 1 || reads[0].Input["directionId"] != "d" {
		t.Fatalf("researchCredentials calls = %v", reads)
	}
	if _, err := gw.Session(context.Background(), gateway.SessionRef{Kind: "plan"}, nodeID); err == nil {
		t.Fatal("a ref naming nothing must be an error")
	}
}

// A platform without the read or the ledger answers IsUnsupported: the
// node's cue to fall back, not to fail. A platform with the item's read
// but not yet the plan's falls back for plans alone.
func TestCredentialsUnsupported(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Unsupported["itemCredentials"] = true
	f.Unsupported["planCredentials"] = true
	f.Unsupported["recordSessionUsage"] = true
	gw := gateway.New(f.URL(), "rs_test")
	if _, err := gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "plan", GoalID: "g"}, nodeID); !gateway.IsUnsupported(err) {
		t.Fatalf("SessionCredentials (plan) = %v, want unsupported", err)
	}
	if _, err := gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "build", ItemID: itemID}, nodeID); !gateway.IsUnsupported(err) {
		t.Fatalf("SessionCredentials (item) = %v, want unsupported", err)
	}
	if err := gw.RecordSessionUsage(context.Background(), gateway.UsageRecord{Kind: "plan"}); !gateway.IsUnsupported(err) {
		t.Fatalf("RecordSessionUsage = %v, want unsupported", err)
	}

	// today's platform: items served, plans not yet
	today := gatewaytest.New("rs_test")
	defer today.Close()
	today.Unsupported["planCredentials"] = true
	today.SessionCredentials[itemID] = []map[string]any{anthropicSubscription("c1", "max-a", "s1")}
	gw = gateway.New(today.URL(), "rs_test")
	if _, err := gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "plan", GoalID: "g"}, nodeID); !gateway.IsUnsupported(err) {
		t.Fatalf("plan on today's platform = %v, want unsupported", err)
	}
	if got, err := gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "build", ItemID: itemID}, nodeID); err != nil || len(got) != 1 {
		t.Fatalf("item on today's platform = %v, %v", got, err)
	}

	// a refusal — another node's item — is an error, not a fallback
	refusing := gatewaytest.New("rs_test")
	defer refusing.Close()
	refusing.Refuse["itemCredentials"] = "itemCredentials: not yours: you are not the node that holds the item"
	gw = gateway.New(refusing.URL(), "rs_test")
	if _, err := gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "build", ItemID: itemID}, nodeID); err == nil || gateway.IsUnsupported(err) {
		t.Fatalf("a refusal = %v, want a plain error", err)
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
