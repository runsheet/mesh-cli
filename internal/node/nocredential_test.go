package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"sync"
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
	f.SessionCredentials[subject] = []map[string]any{claudeCredential}
}

// claudeCredential is the Claude subscription the fixtures issue.
var claudeCredential = map[string]any{"id": "cred-claude", "label": "max-issued", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": "sk-ant-oat01-ISSUED"}

// issueNothing has the fake's claim-time read issue no credential to
// any subject the test has not issued one.
func issueNothing(f *gatewaytest.Fake) { f.DefaultSessionCredentials = nil }

// testCreds is the node's Credentials on gw, as every session is given
// one (a nil one is a programming error).
// A test's wrapping gateway is unwrapped to the one it embeds.
func testCreds(gw any) *Credentials {
	if cg, ok := gw.(CredentialGateway); ok {
		return &Credentials{GW: cg}
	}
	v := reflect.ValueOf(gw)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		for i := range v.NumField() {
			if f := v.Field(i); v.Type().Field(i).Anonymous && f.CanInterface() {
				if cg, ok := f.Interface().(CredentialGateway); ok {
					return &Credentials{GW: cg}
				}
				return testCreds(f.Interface())
			}
		}
	}
	panic(fmt.Sprintf("testCreds: %T is no CredentialGateway", gw))
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
	issueNothing(f)
	gw := gateway.New(f.URL(), "rs_test")
	store := &NoCredentials{}
	c := &Credentials{GW: gw, NoCredential: store}
	sc := Scope{Login: "acme", Slug: "acme", ID: orgID}
	ref := gateway.SessionRef{Kind: string(harness.KindBuild), ItemID: item1, GoalID: goalID}
	// the operator's own key in the env: never what a session opens
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-OWN")

	for name, rows := range map[string][]map[string]any{
		"codex only": {{"id": "cred-codex", "label": "openai", "vendor": "openai", "kind": "api_key", "serves": []any{"codex"}, "secret": "sk-proj-X"}},
		"nothing":    nil,
	} {
		f.SessionCredentials[item1] = rows
		s, err := c.Open(withSubject(context.Background(), sc, projID, ""), ref, "n", AdapterClaudeCode)
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
		if why, _, held := store.Holds(projID, "", AdapterClaudeCode); !held || why != noCredentialReason {
			t.Fatalf("%s: store holds %q, %v", name, why, held)
		}
	}
	// a Codex session on a Claude-only read names codex
	f.SessionCredentials[item1] = nil
	issueClaude(f, item1)
	_, err := c.Open(withSubject(context.Background(), sc, projID, "mesh"), ref, "n", AdapterCodex)
	if nc, ok := NoCredentialOf(err); !ok || nc.Error() != "org acme's project mesh has no codex credential open to this node" {
		t.Fatalf("codex on a claude-only read = %v", err)
	}
	// a credential attached: the hold is let go
	s, err := c.Open(withSubject(context.Background(), sc, projID, "mesh"), ref, "n", AdapterClaudeCode)
	if err != nil || s.Label != "max-issued" {
		t.Fatalf("issued: %v, %v", s, err)
	}
	s.Cleanup()
	if _, _, held := store.Holds(projID, "", AdapterClaudeCode); held {
		t.Fatal("the hold outlived an issued credential")
	}
}

// A platform without the claim-time read issues nothing: the open is
// ErrNoCredential naming the org, project and adapter — never node.yaml
// nor the operator's own login (adr-005's amendment 2026-10, the
// fallback is gone) — the project is held, and node.log says once per
// run, per adapter and kind, that the platform does not issue them.
func TestUnsupportedReadIsNoCredential(t *testing.T) {
	f := seed(t)
	issueNothing(f)
	f.Unsupported["itemCredentials"] = true
	f.Unsupported["planCredentials"] = true
	logs := &syncBuf{}
	store := &NoCredentials{}
	c := &Credentials{GW: gateway.New(f.URL(), "rs_test"), NoCredential: store, Log: log.New(logs, "", 0)}
	sc := Scope{Login: "acme", Slug: "acme", ID: orgID}
	ctx := withSubject(context.Background(), sc, projID, "mesh")
	build := gateway.SessionRef{Kind: string(harness.KindBuild), ItemID: item1, GoalID: goalID}
	for range 2 {
		s, err := c.Open(ctx, build, "n", AdapterClaudeCode)
		nc, ok := NoCredentialOf(err)
		if !ok || s != nil || nc.Error() != noCredentialReason {
			t.Fatalf("unsupported read: %v, %v; want ErrNoCredential %q", s, err, noCredentialReason)
		}
	}
	if why, _, held := store.Holds(projID, "", AdapterClaudeCode); !held || why != noCredentialReason {
		t.Fatalf("store holds %q, %v", why, held)
	}
	if _, err := c.Open(ctx, gateway.SessionRef{Kind: string(harness.KindPlan), GoalID: goalID}, "n", AdapterClaudeCode); !isNoCredential(err) {
		t.Fatalf("unsupported plan read = %v", err)
	}
	const line = "credentials: the platform does not issue claude-code credentials for build sessions; nothing runs until it does"
	if n := strings.Count(logs.String(), line); n != 1 {
		t.Fatalf("said %d times, want once:\n%s", n, logs)
	}
	if n := strings.Count(logs.String(), "credentials: the platform does not issue claude-code credentials for plan sessions; nothing runs until it does"); n != 1 {
		t.Fatalf("plan's said %d times, want once:\n%s", n, logs)
	}
	if n := strings.Count(logs.String(), "credentials: the platform does not issue"); n != 2 {
		t.Fatalf("%d notices, want one per kind:\n%s", n, logs)
	}
}

// The loop against a platform without itemCredentials starts no
// session: the claim is released with the reason, the project's work is
// held, and the heartbeat's holding.unclaimed carries the reason for
// the goal row (goal-node-says-why-work-waits).
func TestUnsupportedReadLoopHoldsOnHoldingUnclaimed(t *testing.T) {
	f := seed(t)
	issueNothing(f)
	f.Unsupported["itemCredentials"] = true
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(2)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node", Harnesses: []string{"claude-code"}, Capacity: 2, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{}
	logs := &syncBuf{}
	logger := log.New(logs, "", 0)
	store := &NoCredentials{Log: logger}
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
	loop.Tick(ctx)
	loop.wg.Wait()

	if len(h.jobs) != 0 {
		t.Fatalf("%d sessions started", len(h.jobs))
	}
	if f.Item(item1)["status"] != "ready" || len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatalf("item: %v", f.Item(item1))
	}
	if !strings.Contains(logs.String(), "credentials: the platform does not issue claude-code credentials for build sessions; nothing runs until it does") {
		t.Fatalf("no unsupported notice:\n%s", logs)
	}
	hb := f.CallsNamed("heartbeatNode")
	holding, ok := hb[len(hb)-1].Input["holding"].(map[string]any)
	if !ok {
		t.Fatalf("heartbeat holding = %#v", hb[len(hb)-1].Input["holding"])
	}
	u, ok := unclaimedOf(t, holding)[goalID+"/item"]
	if !ok || !strings.Contains(u["reason"].(string), noCredentialReason) {
		t.Fatalf("holding.unclaimed = %#v", holding["unclaimed"])
	}
}

// A build whose read issues no credential releases its item with the
// reason and runs no session.
func TestNoCredentialRunnerReleasesTheItem(t *testing.T) {
	f := seed(t)
	issueNothing(f)
	h := &fakeHarness{}
	r, _, item, _ := runnerOnFake(t, f, h, nil)
	f.SessionCredentials[item1] = nil
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
	issueNothing(f)
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
	issueNothing(f)
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

// A hold past its re-check lets one probe through and renews itself at
// once: the next caller is held until the probe's read resolves it.
func TestNoCredentialHoldLetsOneProbeThrough(t *testing.T) {
	now := time.Now()
	store := &NoCredentials{Now: func() time.Time { return now }}
	store.Note(goalID, &ErrNoCredential{Org: "acme", Project: "mesh", ProjectID: projID, Adapter: AdapterClaudeCode})
	if _, _, ok := store.Holds(projID, "", AdapterClaudeCode); !ok {
		t.Fatal("a fresh hold does not hold")
	}
	now = now.Add(NoCredentialRecheck)
	if _, _, ok := store.Holds(projID, "", AdapterClaudeCode); ok {
		t.Fatal("the probe past the re-check is held")
	}
	for i := 0; i < 3; i++ {
		if why, _, ok := store.Holds(projID, "", AdapterClaudeCode); !ok || why != noCredentialReason {
			t.Fatalf("call %d after the probe: %q, %v; want held until the probe resolves", i, why, ok)
		}
	}
	store.Clear(projID, "", AdapterClaudeCode)
	if _, _, ok := store.Holds(projID, "", AdapterClaudeCode); ok {
		t.Fatal("a cleared hold still holds")
	}
}

// With several ready items of a held project, the re-check claims one
// of them — one read — and leaves the rest unclaimed; the read issuing
// none again renews the hold, so the next tick claims nothing.
func TestNoCredentialRecheckClaimsOneOfSeveral(t *testing.T) {
	f := seed(t)
	issueNothing(f)
	f.WorkItems[item2]["status"], f.WorkItems[item2]["dependsOn"] = "ready", []string{}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(4)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node", Harnesses: []string{"claude-code"}, Capacity: 4, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{}
	logs := &syncBuf{}
	logger := log.New(logs, "", 0)
	now := time.Now()
	store := &NoCredentials{Log: logger, Now: func() time.Time { return now }}
	store.Note(goalID, &ErrNoCredential{Org: "acme", Project: "mesh", ProjectID: projID, Adapter: AdapterClaudeCode})
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: logger, NoCredential: store}
	runner := &Runner{GW: gw, Harness: h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour, Log: logger,
		MCP: MeshContextMCP(f.URL(), "rs_test"), Credentials: &Credentials{GW: gw, NoCredential: store, Log: logger}}
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx) // held: nothing claimed
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimWorkItem")); n != 0 {
		t.Fatalf("claimed %d under the hold", n)
	}
	now = now.Add(NoCredentialRecheck)
	loop.Tick(ctx) // the re-check: one probe of two ready items
	waitFor(t, func() bool { return len(f.CallsNamed("releaseWorkItem")) == 1 && loop.Running() == 0 })
	if n := len(f.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("the re-check claimed %d of 2 ready items, want 1", n)
	}
	loop.Tick(ctx) // the probe's read renewed the hold
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("claimed %d after the probe, want 1", n)
	}
	if len(h.jobs) != 0 {
		t.Fatalf("%d sessions started", len(h.jobs))
	}
	if n := strings.Count(logs.String(), noCredentialReason); n != 1 {
		t.Fatalf("node.log says the reason %d times, want once:\n%s", n, logs)
	}
}

// A rebase whose read issues no credential waits: no session, no
// failWorkItemRebase handing the item to its builder, the reason said
// once for the goal however many ticks pass, and no second claim while
// the hold lasts.
func TestNoCredentialRebaseWaitsAndSaysWhyOnce(t *testing.T) {
	f := seed(t)
	issueNothing(f)
	requestRebase(f, item1, "mesh/ship-it-1")
	f.RebaseTokens = true
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node"}); err != nil {
		t.Fatal(err)
	}
	ws := &fakeWS{root: t.TempDir(), pushedHead: "abc123"}
	h := &fakeHarness{text: "REBASED: abc123\nRESOLVED: main.go"}
	logs := &syncBuf{}
	logger := log.New(logs, "", 0)
	store := &NoCredentials{Log: logger}
	rb := &Rebaser{GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MCP: MeshContextMCP(f.URL(), "rs_test"), MaxBudgetUSD: 4, ProgressEvery: time.Hour, Log: logger,
		Credentials: &Credentials{GW: gw, NoCredential: store, Log: logger}}
	var errs []error
	var mu sync.Mutex
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: logger, NoCredential: store,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Rebase: func(ctx context.Context, item gateway.WorkItem) {
			err := rb.Rebase(ctx, item)
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		loop.Tick(ctx)
		loop.wg.Wait()
	}
	if len(h.jobs) != 0 {
		t.Fatalf("%d sessions started", len(h.jobs))
	}
	if n := len(f.CallsNamed("failWorkItemRebase")); n != 0 {
		t.Fatalf("the rebase was failed %d times: no credential is a wait", n)
	}
	if n := len(f.CallsNamed("claimWorkItemRebase")); n != 1 {
		t.Fatalf("claimed the rebase %d times, want once (the read that found none)", n)
	}
	if len(errs) != 1 || !isNoCredential(errs[0]) {
		t.Fatalf("Rebase = %v, want one ErrNoCredential", errs)
	}
	if n := strings.Count(logs.String(), noCredentialReason); n != 1 {
		t.Fatalf("node.log says the reason %d times, want once:\n%s", n, logs)
	}
	if strings.Contains(logs.String(), "rebase failed") {
		t.Fatalf("logged as a failure:\n%s", logs)
	}
}

// A Codex session has no own account to fall back to (ADR 011's
// amendment 2026-10-02): a read that answered and issued no Codex
// credential is ErrNoCredential naming codex, and no account is opened.
func TestNoCredentialCodexOpenOnAnAnsweredReadWithNoCodex(t *testing.T) {
	f := seed(t)
	issueNothing(f)
	issueClaude(f, item1)
	c := &Credentials{GW: gateway.New(f.URL(), "rs_test"), NoCredential: &NoCredentials{}}
	sc := Scope{Login: "acme", Slug: "acme", ID: orgID}
	ref := gateway.SessionRef{Kind: string(harness.KindEvaluate), ItemID: item1, GoalID: goalID}
	s, err := c.Open(withSubject(context.Background(), sc, projID, "mesh"), ref, "n", AdapterCodex)
	if nc, ok := NoCredentialOf(err); !ok || s != nil || nc.Error() != "org acme's project mesh has no codex credential open to this node" {
		t.Fatalf("Open(codex) = %v, %v; want ErrNoCredential naming codex", s, err)
	}
}

// An evaluation whose only Codex candidate the read does not issue, on
// a read that issues Claude alone, starts no Codex session: unpinned it runs on
// the issued Claude credential, pinned to Codex it waits — the
// operator's ~/.codex login never runs it.
func TestNoCredentialEvaluationNeverRunsCodexOnItsOwnAccount(t *testing.T) {
	ownOnly := func(ev *Evaluator, f *gatewaytest.Fake) {
		f.SessionCredentials[item1] = nil
		issueClaude(f, item1)
		ev.CodexAccounts = func() []AccountConfig {
			return []AccountConfig{{Label: DefaultAccount, Kind: AccountSubscription, Adapter: AdapterCodex}}
		}
	}
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: PASS")
	ownOnly(ev, f)
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 0 || len(claude.jobs) != 1 {
		t.Fatalf("unpinned: runs claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	if u := usageOf(t, f); u["adapter"] != "claude-code" || u["credentialId"] != "cred-claude" || in["passed"] != true {
		t.Fatalf("unpinned: usage %v, recorded %v", u, in)
	}

	f, ev, claude, codex, _, _ = familyFixture(t, "normal", "sonnet", "VERDICT: PASS", "VERDICT: PASS")
	ownOnly(ev, f)
	f.WorkItems[item1]["evaluateModel"] = "gpt-6-sol"
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ev.Evaluate(context.Background(), *item); !errors.Is(err, ErrFamilyWaits) {
			t.Fatalf("pinned: Evaluate = %v, want ErrFamilyWaits", err)
		}
	}
	if codex.runs() != 0 || len(claude.jobs) != 0 || len(f.CallsNamed("evaluateWorkItem")) != 0 {
		t.Fatalf("pinned: runs claude %d, codex %d", len(claude.jobs), codex.runs())
	}
}
