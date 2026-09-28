package gateway_test

import (
	"context"
	"testing"

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
		{ID: "c1", Label: "max-a", Kind: "subscription", Adapter: "claude-code", SecretVersion: 1, Secret: "sk-ant-oat01-x"},
		{ID: "c2", Label: "key", Kind: "api", Adapter: "claude-code", SecretVersion: 2, Secret: "sk-ant-api-y", CapDailyUSD: 50, CapMonthlyUSD: 1000},
		{ID: "c3", Label: "oauth", Kind: "subscription", Adapter: "claude-code", Secret: "sk-ant-oat01-z"},
		{ID: "c6", Label: "by-calls", Kind: "api", Adapter: "claude-code", Secret: "sk-ant-api-calls"},
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

// An OpenAI credential whose serves name codex is a Codex account: an
// oauth_token (the ChatGPT login's auth.json) is a subscription, an
// api_key an api key with the row's caps. An OpenAI row serving only a
// tool is that tool's account, not a session's.
func TestSessionCredentialsCodex(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	login := `{"tokens":{"refresh_token":"rt-codex-FIXTURE"}}`
	f.SessionCredentials[itemID] = []map[string]any{
		{"id": "o1", "label": "chatgpt-pro", "vendor": "openai", "kind": "oauth_token", "serves": []any{"codex"}, "secretVersion": 3.0, "secret": login},
		{"id": "o2", "label": "openai-key", "vendor": "openai", "kind": "api_key", "serves": []any{"codex", "evaluate"}, "capUnit": "dollars", "dailyCap": 30.0, "monthlyCap": 600.0, "secretVersion": 1.0, "secret": "sk-openai-FIXTURE"},
		{"id": "o3", "label": "openai-tool", "vendor": "openai", "kind": "api_key", "serves": []any{"embeddings"}, "secret": "sk-openai-tool"},
		{"id": "o4", "label": "openai-none", "vendor": "openai", "kind": "api_key", "serves": []any{}, "secret": "sk-openai-none"},
	}
	gw := gateway.New(f.URL(), "rs_test")
	got, err := gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "evaluate", ItemID: itemID, GoalID: "g"}, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	want := []gateway.Credential{
		{ID: "o1", Label: "chatgpt-pro", Kind: "subscription", Adapter: "codex", SecretVersion: 3, Secret: login},
		{ID: "o2", Label: "openai-key", Kind: "api", Adapter: "codex", SecretVersion: 1, Secret: "sk-openai-FIXTURE", CapDailyUSD: 30, CapMonthlyUSD: 600},
	}
	if len(got) != len(want) {
		t.Fatalf("credentials = %#v, want %d", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("credential %d = %#v, want %#v", i, got[i], want[i])
		}
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

// A platform without the read answers IsUnsupported: the
// node's cue to fall back, not to fail. A platform with the item's read
// but not yet the plan's falls back for plans alone.
func TestCredentialsUnsupported(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Unsupported["itemCredentials"] = true
	f.Unsupported["planCredentials"] = true
	gw := gateway.New(f.URL(), "rs_test")
	if _, err := gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "plan", GoalID: "g"}, nodeID); !gateway.IsUnsupported(err) {
		t.Fatalf("SessionCredentials (plan) = %v, want unsupported", err)
	}
	if _, err := gw.SessionCredentials(context.Background(), gateway.SessionRef{Kind: "build", ItemID: itemID}, nodeID); !gateway.IsUnsupported(err) {
		t.Fatalf("SessionCredentials (item) = %v, want unsupported", err)
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
