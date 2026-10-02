package node

import (
	"context"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// issueClaude has the fake's claim-time read issue subject one Claude
// subscription, unless a test has issued it some already: a session
// runs only on an issued credential (ErrNoCredential).
func issueClaude(f *gatewaytest.Fake, subject string) {
	if len(f.SessionCredentials[subject]) > 0 {
		return
	}
	f.SessionCredentials[subject] = []map[string]any{{"id": "cred-claude", "label": "max-issued", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": "sk-ant-oat01-ISSUED"}}
}

// noCredentialReason is what seed's org and project say when the read
// issues no Claude credential.
const noCredentialReason = "org acme's project mesh has no claude-code credential open to this node"

// A read that answers and issues nothing for the session's adapter —
// only a Codex credential, or nothing at all — is ErrNoCredential naming
// the org, the project (read by id when the caller has no slug) and the
// adapter, and opens no account: never node.yaml's, never the
// operator's own login (the latchkeyid planner's 512 starts on
// 2026-09-30).
func TestNoCredentialOpenNamesOrgProjectAndAdapter(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	store := &NoCredentials{}
	c := &Credentials{GW: gw, NoCredential: store}
	sc := Scope{Login: "acme", Slug: "acme", ID: orgID}
	ref := gateway.SessionRef{Kind: string(harness.KindBuild), ItemID: item1, GoalID: goalID}
	// a fallback that would open if Open ever fell to it
	fallback := []AccountConfig{{Label: "own", Kind: AccountAPI, APIKeyEnv: "MESH_TEST_OWN_KEY"}}
	t.Setenv("MESH_TEST_OWN_KEY", "sk-ant-api03-OWN")

	for name, rows := range map[string][]map[string]any{
		"codex only": {{"id": "cred-codex", "label": "openai", "vendor": "openai", "kind": "api_key", "serves": []any{"codex"}, "secret": "sk-proj-X"}},
		"nothing":    nil,
	} {
		f.SessionCredentials[item1] = rows
		s, err := c.Open(withSubject(context.Background(), sc, projID, ""), ref, "n", fallback, AdapterClaudeCode)
		nc, ok := NoCredentialOf(err)
		if !ok {
			t.Fatalf("%s: Open = %v, %v; want ErrNoCredential", name, s, err)
		}
		if s != nil {
			t.Fatalf("%s: an account was opened: %+v", name, s)
		}
		if nc.Error() != noCredentialReason || nc.Org != "acme" || nc.Project != "mesh" || nc.Adapter != "claude-code" {
			t.Fatalf("%s: %+v: %q", name, nc, nc.Error())
		}
		if why, held := store.Holds(projID, AdapterClaudeCode); !held || why != noCredentialReason {
			t.Fatalf("%s: store holds %q, %v", name, why, held)
		}
	}
	// a Codex session on a Claude-only read names codex
	f.SessionCredentials[item1] = nil
	issueClaude(f, item1)
	_, err := c.Open(withSubject(context.Background(), sc, projID, "mesh"), ref, "n", nil, AdapterCodex)
	if nc, ok := NoCredentialOf(err); !ok || nc.Error() != "org acme's project mesh has no codex credential open to this node" {
		t.Fatalf("codex on a claude-only read = %v", err)
	}
	// a credential attached: the hold is let go
	s, err := c.Open(withSubject(context.Background(), sc, projID, "mesh"), ref, "n", nil, AdapterClaudeCode)
	if err != nil || s.Label != "max-issued" {
		t.Fatalf("issued: %v, %v", s, err)
	}
	s.Cleanup()
	if _, held := store.Holds(projID, AdapterClaudeCode); held {
		t.Fatal("the hold outlived an issued credential")
	}
}

// A platform without the read still runs on node.yaml's accounts: the
// fallback is for an unsupported read alone.
func TestNoCredentialNotForAnUnsupportedRead(t *testing.T) {
	f := seed(t)
	f.Unsupported["itemCredentials"] = true
	c := &Credentials{GW: gateway.New(f.URL(), "rs_test")}
	ref := gateway.SessionRef{Kind: string(harness.KindBuild), ItemID: item1, GoalID: goalID}
	s, err := c.Open(context.Background(), ref, "n", nil, AdapterClaudeCode)
	if err != nil || s == nil {
		t.Fatalf("unsupported read: %v, %v", s, err)
	}
}

// A build whose read issues no credential releases its item with the
// reason and runs no session.
func TestNoCredentialRunnerReleasesTheItem(t *testing.T) {
	f := seed(t)
	h := &fakeHarness{}
	r, _, item, _ := runnerOnFake(t, f, h, nil)
	r.Credentials = &Credentials{GW: r.GW.(*gateway.Client), Log: r.Log}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 0 {
		t.Fatalf("%d sessions started", len(h.jobs))
	}
	rel := f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 || !strings.Contains(rel[0].Input["note"].(string), noCredentialReason) {
		t.Fatalf("release: %+v", rel)
	}
	if f.Item(item1)["status"] != "ready" || len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatalf("item: %v", f.Item(item1))
	}
}

// The loop on a project with no Claude credential open to the node:
// the first tick's claim is released, the later ticks claim nothing,
// no session starts in any, node.log says the reason once for the goal,
// and the heartbeat's holding.unclaimed names the goal with it. Ten
// minutes on, the loop asks again.
func TestNoCredentialLoopHoldsTheGoalAndSaysWhyOnce(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(2)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node", Harnesses: []string{"claude-code"}, Capacity: 2, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{}
	logs := &syncBuf{}
	logger := log.New(logs, "", 0)
	now := time.Now()
	clock := func() time.Time { return now }
	store := &NoCredentials{Log: logger, Now: clock}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: logger, NoCredential: store}
	runner := &Runner{GW: gw, Harness: h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour, Log: logger,
		MCP: MeshContextMCP(f.URL(), "rs_test"), Credentials: &Credentials{GW: gw, NoCredential: store, Log: logger}}
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	waitFor(t, func() bool { return len(f.CallsNamed("releaseWorkItem")) == 1 && loop.Running() == 0 })
	loop.Tick(ctx)
	loop.wg.Wait()
	// the third tick's heartbeat carries the second's pass
	loop.Tick(ctx)
	loop.wg.Wait()

	if len(h.jobs) != 0 {
		t.Fatalf("%d sessions started", len(h.jobs))
	}
	if n := len(f.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("claimed %d times, want once (the read that found none)", n)
	}
	if n := strings.Count(logs.String(), noCredentialReason); n != 1 {
		t.Fatalf("node.log says the reason %d times, want once:\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "accounts: "+noCredentialReason) {
		t.Fatalf("not in the accounts: family:\n%s", logs)
	}
	hb := f.CallsNamed("heartbeatNode")
	holding, ok := hb[len(hb)-1].Input["holding"].(map[string]any)
	if !ok {
		t.Fatalf("heartbeat holding = %#v", hb[len(hb)-1].Input["holding"])
	}
	un := unclaimedOf(t, holding)
	u, ok := un[goalID+"/item"]
	if len(un) != 1 || !ok {
		t.Fatalf("holding.unclaimed = %#v", holding["unclaimed"])
	}
	if u["count"] != float64(1) || !strings.Contains(u["reason"].(string), noCredentialReason) {
		t.Fatalf("unclaimed = %#v", u)
	}
	if s, _ := holding["summary"].(string); !strings.Contains(s, "1 item(s) and 0 plan(s) left unclaimed") {
		t.Fatalf("summary = %q", s)
	}

	// an admin attaches a credential; past the re-check the loop asks
	// again and the item runs
	issueClaude(f, item1)
	now = now.Add(NoCredentialRecheck)
	loop.Tick(ctx)
	waitFor(t, func() bool { return f.Item(item1)["status"] == "evaluating" })
	if len(h.jobs) != 1 {
		t.Fatalf("%d sessions after the credential was attached", len(h.jobs))
	}
}

// A goal in planning in a held project is not claimed for planning, nor
// its ready items: each passed over with the reason, said once.
func TestNoCredentialLoopHoldsThePlan(t *testing.T) {
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(2)}
	logs := &syncBuf{}
	store := &NoCredentials{Log: log.New(logs, "", 0)}
	store.Note(goalID, &ErrNoCredential{Org: "acme", Project: "mesh", ProjectID: projID, Adapter: AdapterClaudeCode})
	var planned atomic.Int32
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: log.New(os.Stderr, "", 0), NoCredential: store,
		Plan: func(context.Context, gateway.Goal, Scope) { planned.Add(1) }}
	loop.Tick(context.Background())
	loop.Tick(context.Background())
	loop.wg.Wait()
	if planned.Load() != 0 || len(f.CallsNamed("claimPlan")) != 0 {
		t.Fatalf("planned %d, claims %d", planned.Load(), len(f.CallsNamed("claimPlan")))
	}
	// the goal's plan and its ready item, each held with the reason
	h := loop.Holding()
	if h == nil || len(h.Unclaimed) != 2 {
		t.Fatalf("holding = %+v", h)
	}
	for _, u := range h.Unclaimed {
		if u.GoalID != goalID || !strings.Contains(u.Reason, noCredentialReason) {
			t.Fatalf("unclaimed = %+v", u)
		}
	}
	if len(f.CallsNamed("claimWorkItem")) != 0 {
		t.Fatal("the held project's item was claimed")
	}
	if n := strings.Count(logs.String(), noCredentialReason); n != 1 {
		t.Fatalf("said %d times:\n%s", n, logs)
	}
}
