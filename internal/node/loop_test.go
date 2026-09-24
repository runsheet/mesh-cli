package node

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

const (
	orgID  = "0f3b8a1e-2c4d-4e6f-8a9b-0c1d2e3f4a5b"
	acctID = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	projID = "bbbbbbbb-1111-4111-8111-bbbbbbbbbbbb"
	repoID = "cccccccc-1111-4111-8111-cccccccccccc"
	goalID = "dddddddd-1111-4111-8111-dddddddddddd"
	item1  = "eeeeeeee-1111-4111-8111-eeeeeeeeeeee"
	item2  = "ffffffff-1111-4111-8111-ffffffffffff"
)

// Seed fills a fake with one org, project, repo, goal and two items (the
// first ready, the second pending).
func seed(t *testing.T) *gatewaytest.Fake {
	t.Helper()
	f := gatewaytest.New("rs_test")
	f.Orgs = []map[string]any{{"id": orgID, "login": "acme", "accountId": acctID}}
	f.Accounts[acctID] = map[string]any{"id": acctID, "login": "acme"}
	f.Projects[projID] = map[string]any{"id": projID, "orgId": orgID, "slug": "mesh", "name": "Mesh"}
	f.Repos[repoID] = map[string]any{"id": repoID, "accountId": acctID, "name": "widgets", "defaultBranch": "main"}
	f.Goals[goalID] = map[string]any{"id": goalID, "orgId": orgID, "projectId": projID, "slug": "ship-it", "title": "Ship it",
		"statement": "Make the thing.", "acceptance": "go test ./... passes", "budgetCents": 1000, "spentCents": 100, "status": "running"}
	f.WorkItems[item1] = map[string]any{"id": item1, "goalId": goalID, "orgId": orgID, "seq": 1, "title": "Write the failing test",
		"brief": "A test for the thing.", "acceptance": "the test fails", "repoId": repoID, "baseRef": "main", "status": "ready", "dependsOn": []string{}}
	f.WorkItems[item2] = map[string]any{"id": item2, "goalId": goalID, "orgId": orgID, "seq": 2, "title": "Make it pass",
		"brief": "Implement.", "acceptance": "go test passes", "repoId": repoID, "baseRef": "main", "status": "pending", "dependsOn": []string{item1}}
	f.Documents["acme/mesh"] = []map[string]any{
		{"slug": "adr-001", "title": "ADR 001", "kind": "adr", "repo": "acme/widgets", "path": "specs/adr-001.md"},
		{"slug": "prd", "title": "PRD", "kind": "prd", "repo": "acme/widgets", "path": "specs/prd.md"},
		{"slug": "agents", "title": "AGENTS.md", "kind": "instructions", "repo": "acme/widgets", "path": "AGENTS.md"},
		{"slug": "notes", "title": "Notes", "kind": "spec", "repo": "acme/widgets", "path": "specs/notes.md"},
	}
	f.Bodies["acme/mesh/adr-001"] = "# ADR 001"
	f.Bodies["acme/mesh/prd"] = "# PRD"
	f.Bodies["acme/mesh/agents"] = "# AGENTS"
	t.Cleanup(f.Close)
	return f
}

// scopesOf is the one-org scope an older config describes.
func scopesOf(cfg *Config) Scopes {
	return Scopes{{Login: cfg.OrgLogin, ID: cfg.OrgID, AccountID: cfg.AccountID, NodeID: cfg.NodeID}}
}

type fakeWS struct {
	mu       sync.Mutex
	root     string
	prepared []WorkspaceSpec
	pushed   []string
	commits  []string
	fetched  []string
}

func (w *fakeWS) Prepare(_ context.Context, spec WorkspaceSpec) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prepared = append(w.prepared, spec)
	dir := spec.Dir()
	return dir, os.MkdirAll(dir, 0o755)
}

func (w *fakeWS) Commit(_ context.Context, dir string, paths []string, msg string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commits = append(w.commits, msg+" ["+strings.Join(paths, ",")+"]")
	return nil
}

func (w *fakeWS) Push(_ context.Context, dir, branch string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pushed = append(w.pushed, branch)
	return "abc123def456", nil
}

func (w *fakeWS) Fetch(_ context.Context, repo, dir string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fetched = append(w.fetched, repo+" → "+dir)
	return os.MkdirAll(dir, 0o755)
}

func (w *fakeWS) Diff(_ context.Context, dir, base string) (string, error) {
	return "+++ b/thing_test.go\n+func TestThing(t *testing.T) {}", nil
}

type fakeHarness struct {
	mu   sync.Mutex
	jobs []harness.Job
	fail bool
	// text is the final message when set (an evaluator's verdict).
	text string
}

func (h *fakeHarness) Name() string { return "claude-code" }
func (h *fakeHarness) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	h.jobs = append(h.jobs, job)
	h.mu.Unlock()
	if on == nil {
		on = func(harness.Progress) {}
	}
	on(harness.Progress{SessionID: "s-1"})
	on(harness.Progress{Text: "wrote the test", CostUSD: 0.5})
	if h.fail {
		return harness.Result{Success: false, Text: "could not", CostUSD: 0.75, SessionID: "s-1"}, nil
	}
	on(harness.Progress{Text: "done", CostUSD: 1.25})
	text := "Implemented and tested."
	if h.text != "" {
		text = h.text
	}
	return harness.Result{Success: true, Text: text, CostUSD: 1.25, SessionID: "s-1"}, nil
}

func TestLoopClaimsRunsAndFinishes(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: 2}
	if err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node", Harnesses: []string{"claude-code"}, Capacity: 2, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{}
	runner := &Runner{GW: gw, Harness: h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, MaxBudgetUSD: 5, ProgressEvery: 10 * time.Millisecond, Log: log.New(os.Stderr, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: runner.Log,
		Dispatch: func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) }}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	waitFor(t, func() bool { return f.Item(item1)["status"] == "evaluating" })

	row := f.Item(item1)
	if row["branch"] != "mesh/ship-it-1" || row["headSha"] != "abc123def456" || row["nodeId"] != cfg.NodeID {
		t.Fatalf("finished row: %v", row)
	}
	if got := row["costCents"]; got != 125 {
		t.Fatalf("cost folded = %v, want 125 (deltas: progress + finish)", got)
	}
	if f.Item(item2)["status"] != "pending" {
		t.Fatal("the pending item must not be claimed")
	}
	if hb := f.CallsNamed("heartbeatNode"); len(hb) != 1 || hb[0].Input["running"] != float64(0) {
		t.Fatalf("heartbeat calls: %+v", hb)
	}
	progress := f.CallsNamed("reportWorkItemProgress")
	if len(progress) < 2 || progress[0].Input["sessionId"] != "" || progress[len(progress)-1].Input["sessionId"] != "s-1" {
		t.Fatalf("progress calls: %+v", progress)
	}
	fin := f.CallsNamed("finishWorkItem")
	if len(fin) != 1 || fin[0].Input["branch"] != "mesh/ship-it-1" {
		t.Fatalf("finish calls: %+v", fin)
	}
	if len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatal("must not fail a finished item")
	}

	// the workspace, the branch and the budget
	if len(ws.prepared) != 1 || ws.prepared[0].Repo != "acme/widgets" || ws.prepared[0].Branch != "mesh/ship-it-1" || ws.prepared[0].BaseRef != "main" {
		t.Fatalf("prepared: %+v", ws.prepared)
	}
	if len(ws.pushed) != 1 || ws.pushed[0] != "mesh/ship-it-1" {
		t.Fatalf("pushed: %v", ws.pushed)
	}
	if len(h.jobs) != 1 || h.jobs[0].MaxBudgetUSD != 5 || h.jobs[0].Kind != harness.KindBuild || !strings.Contains(h.jobs[0].Prompt, "Write the failing test") {
		t.Fatalf("job: %+v", h.jobs)
	}
	// the context: instructions, adr and prd written; the spec skipped
	dir := ws.prepared[0].Dir()
	entries, _ := os.ReadDir(filepath.Join(dir, ContextDir))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "INDEX.md,adr-adr-001.md,instructions-acme-widgets-agents.md,prd-prd.md" {
		t.Fatalf("context files: %v", names)
	}
	if !strings.Contains(h.jobs[0].Prompt, "instructions-acme-widgets-agents.md, adr-adr-001.md, prd-prd.md") {
		t.Fatalf("prompt must list the context in reading order:\n%s", h.jobs[0].Prompt)
	}
	// the index lists every document, the spec too, and says which are on disk
	index, _ := os.ReadFile(filepath.Join(dir, ContextDir, IndexFile))
	for _, want := range []string{"Project acme/mesh", "| adr | ADR 001 |  | `adr-001` | acme/widgets/specs/adr-001.md | `adr-adr-001.md` |", "| spec | Notes |  | `notes` | acme/widgets/specs/notes.md | fetch |"} {
		if !strings.Contains(string(index), want) {
			t.Fatalf("index lacks %q:\n%s", want, index)
		}
	}
	// no MCP on this runner: no Prologue rule in the prompt
	if strings.Contains(h.jobs[0].Prompt, "prologue_document") || h.jobs[0].MCPServers != nil {
		t.Fatal("no MCP server was given, none should be promised")
	}
	// the goal document, committed on the branch with the PRD as parent
	doc, err := os.ReadFile(GoalDocPath(dir, "ship-it"))
	if err != nil || !strings.Contains(string(doc), "parent: prd\n") || !strings.Contains(string(doc), "· item 1 · Implemented and tested.") {
		t.Fatalf("goal doc: %v\n%s", err, doc)
	}
	if len(ws.commits) != 2 || !strings.HasPrefix(ws.commits[0], "docs(mesh): goal ship-it — item 1 [specs/goal-ship-it.md]") {
		t.Fatalf("commits: %v", ws.commits)
	}
	// a second tick claims nothing new and reports nothing running
	loop.Tick(ctx)
	if hb := f.CallsNamed("heartbeatNode"); len(hb) != 2 || len(f.CallsNamed("claimWorkItem")) != 1 {
		t.Fatalf("second tick: heartbeats=%d claims=%d", len(hb), len(f.CallsNamed("claimWorkItem")))
	}
}

func TestLoopSkipsRefusedClaim(t *testing.T) {
	f := seed(t)
	f.Refuse["claimWorkItem"] = "item is claimed, not ready"
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: 1}
	_ = gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "n"})
	dispatched := 0
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Dispatch: func(context.Context, gateway.WorkItem) { dispatched++ }}
	loop.Tick(context.Background())
	if dispatched != 0 || loop.Running() != 0 {
		t.Fatalf("a refused claim must not run: dispatched=%d running=%d", dispatched, loop.Running())
	}
}

func TestRunnerFailsAnUnsuccessfulSession(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: 1}
	if err := gw.ClaimWorkItem(context.Background(), item1, cfg.NodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	ws := &fakeWS{root: t.TempDir()}
	runner := &Runner{GW: gw, Harness: &fakeHarness{fail: true}, WS: ws, Cfg: cfg, WorkRoot: ws.root, ProgressEvery: time.Hour}
	var item gateway.WorkItem
	item.ID, item.GoalID, item.RepoID, item.Seq, item.Title = item1, goalID, repoID, 1, "Write the failing test"
	if err := runner.RunItem(context.Background(), item); err == nil {
		t.Fatal("expected an error")
	}
	if row := f.Item(item1); row["status"] != "failed" || !strings.Contains(row["lastError"].(string), "could not") {
		t.Fatalf("row: %v", row)
	}
	if len(ws.pushed) != 0 {
		t.Fatal("a failed item must not be pushed")
	}
	// the goal's remaining budget bounds the session: (1000-100)/100 = 9
	if b := runner.Budget(&gateway.Goal{BudgetCents: 1000, SpentCents: 100}); b != 9 {
		t.Fatalf("budget = %v", b)
	}
	runner.MaxBudgetUSD = 2
	if b := runner.Budget(&gateway.Goal{BudgetCents: 1000, SpentCents: 100}); b != 2 {
		t.Fatalf("capped budget = %v", b)
	}
}

func TestRunnerLeavesItemOnShutdown(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: 1}
	_ = gw.ClaimWorkItem(context.Background(), item1, cfg.NodeID, "claude-code")
	ws := &fakeWS{root: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	blocking := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		cancel()
		<-ctx.Done()
		return harness.Result{}, ctx.Err()
	})
	runner := &Runner{GW: gw, Harness: blocking, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour}
	var item gateway.WorkItem
	item.ID, item.GoalID, item.RepoID, item.Seq = item1, goalID, repoID, 1
	if err := runner.RunItem(ctx, item); err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	if row := f.Item(item1); row["status"] == "failed" {
		t.Fatal("our own shutdown must not fail the item")
	}
}

type harnessFunc func(context.Context, harness.Job, func(harness.Progress)) (harness.Result, error)

func (h harnessFunc) Name() string { return "fake" }
func (h harnessFunc) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	return h(ctx, job, on)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func mustUUID(s string) uuid.UUID { return uuid.MustParse(s) }

// One node, two orgs: registered in each, heartbeating each, claiming
// from either under one capacity; an org the config names that the
// owner is not in is refused at resolution.
func TestLoopServesEveryOrgOfItsOwner(t *testing.T) {
	f := seed(t)
	const org2, acct2, goal2, item3 = "11111111-2222-4333-8444-555555555555", "aaaaaaaa-2222-4222-8222-aaaaaaaaaaaa", "dddddddd-2222-4222-8222-dddddddddddd", "eeeeeeee-2222-4222-8222-eeeeeeeeeeee"
	f.Orgs = append(f.Orgs, map[string]any{"id": org2, "login": "globex", "accountId": acct2})
	f.Accounts[acct2] = map[string]any{"id": acct2, "login": "globex"}
	f.Goals[goal2] = map[string]any{"id": goal2, "orgId": org2, "projectId": projID, "slug": "other", "title": "Other", "budgetCents": 1000, "status": "running"}
	f.WorkItems[item3] = map[string]any{"id": item3, "goalId": goal2, "orgId": org2, "seq": 1, "title": "Globex work",
		"brief": "Do it.", "acceptance": "done", "repoId": repoID, "baseRef": "main", "status": "ready", "dependsOn": []string{}}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	cfg := &Config{Slug: "box", Name: "Box", Capacity: 1}
	scopes, err := ResolveScopes(ctx, gw, cfg)
	if err != nil || len(scopes) != 2 || scopes[0].Login != "acme" || scopes[1].Login != "globex" || scopes[0].NodeID == scopes[1].NodeID {
		t.Fatalf("scopes = %+v, %v", scopes, err)
	}
	if _, err := ResolveScopes(ctx, gw, &Config{Slug: "box", Orgs: []string{"acme", "initech"}}); err == nil || !strings.Contains(err.Error(), "initech") {
		t.Fatalf("an org the owner is not in must refuse: %v", err)
	}
	only, err := ResolveScopes(ctx, gw, &Config{Slug: "box", Orgs: []string{"Globex"}})
	if err != nil || len(only) != 1 || only[0].ID != org2 {
		t.Fatalf("restricted scopes = %+v, %v", only, err)
	}

	for _, sc := range scopes {
		if err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: sc.NodeID, OrgID: sc.ID, OwnerID: f.UserID, Name: cfg.Name, Slug: cfg.Slug}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var ran []string
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopes, Interval: time.Hour, Harness: "claude-code",
		Dispatch: func(_ context.Context, item gateway.WorkItem) { mu.Lock(); ran = append(ran, item.ID); mu.Unlock() }}
	loop.Tick(ctx)
	loop.wg.Wait()
	if hb := f.CallsNamed("heartbeatNode"); len(hb) != 2 || hb[0].Input["aggregateId"] != scopes[0].NodeID || hb[1].Input["aggregateId"] != scopes[1].NodeID {
		t.Fatalf("heartbeats: %+v", hb)
	}
	// capacity 1: acme's item first, globex's waits for the next tick
	if len(ran) != 1 || ran[0] != item1 || f.Item(item1)["nodeId"] != scopes[0].NodeID || f.Item(item3)["status"] != "ready" {
		t.Fatalf("first tick ran %v; item3 %v", ran, f.Item(item3)["status"])
	}
	loop.Tick(ctx)
	loop.wg.Wait()
	if len(ran) != 2 || ran[1] != item3 || f.Item(item3)["nodeId"] != scopes[1].NodeID {
		t.Fatalf("second tick ran %v; item3 node %v", ran, f.Item(item3)["nodeId"])
	}
}

// A poll-mode build takes its Prologue bearer per item: the node's
// machine access token expires within the hour, so the map captured when
// the node started would hand a long-running node's later sessions a
// dead token (latchkey adr-machine-identity, rollout phase 2).
func TestRunnerAsksForItsMCPServersPerItem(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: 1}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{}
	calls := 0
	runner := &Runner{GW: gw, Harness: h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour,
		MCP: PrologueMCP(f.URL(), "stale"),
		MCPFunc: func(context.Context) map[string]any {
			calls++
			return PrologueMCP(f.URL(), fmt.Sprintf("machine-token-%d", calls))
		}}

	f.WorkItems[item2]["status"] = "ready"
	for seq, id := range []string{item1, item2} {
		var item gateway.WorkItem
		item.ID, item.GoalID, item.RepoID, item.Seq, item.Title = id, goalID, repoID, seq+1, "Write the failing test"
		if err := gw.ClaimWorkItem(context.Background(), id, cfg.NodeID, "claude-code"); err != nil {
			t.Fatal(err)
		}
		if err := runner.RunItem(context.Background(), item); err != nil {
			t.Fatalf("item %d: %v", seq+1, err)
		}
	}
	if calls != 2 || len(h.jobs) != 2 {
		t.Fatalf("MCPFunc calls = %d, jobs = %d, want 2 and 2", calls, len(h.jobs))
	}
	for i, want := range []string{"Bearer machine-token-1", "Bearer machine-token-2"} {
		srv, _ := h.jobs[i].MCPServers["prologue"].(map[string]any)
		headers, _ := srv["headers"].(map[string]string)
		if headers["Authorization"] != want {
			t.Fatalf("job %d Authorization = %q, want %q (MCP must not be the map from node start)", i, headers["Authorization"], want)
		}
	}
	// the prompt promises Prologue because MCPFunc gave a server
	if !strings.Contains(h.jobs[0].Prompt, "prologue_document") {
		t.Fatal("a session with Prologue must be told about it")
	}
}

// priorityFake is two orgs, acme and globex, with no goals or items:
// each test adds its own with addGoal and addItem.
type priorityFake struct {
	*gatewaytest.Fake
	t      *testing.T
	scopes Scopes
	n      int
}

const org2, acct2 = "11111111-2222-4333-8444-555555555555", "aaaaaaaa-2222-4222-8222-aaaaaaaaaaaa"

func newPriorityFake(t *testing.T) *priorityFake {
	f := seed(t)
	delete(f.Goals, goalID)
	delete(f.WorkItems, item1)
	delete(f.WorkItems, item2)
	f.Orgs = append(f.Orgs, map[string]any{"id": org2, "login": "globex", "accountId": acct2})
	f.Accounts[acct2] = map[string]any{"id": acct2, "login": "globex"}
	gw := gateway.New(f.URL(), "rs_test")
	scopes, err := ResolveScopes(context.Background(), gw, &Config{Slug: "box", Name: "Box", Capacity: 1})
	if err != nil || len(scopes) != 2 {
		t.Fatalf("scopes = %+v, %v", scopes, err)
	}
	return &priorityFake{Fake: f, t: t, scopes: scopes}
}

// addGoal adds a running goal of that priority to the org; its id is slug.
func (p *priorityFake) addGoal(org, slug, priority string) {
	p.Goals[slug] = map[string]any{"id": slug, "orgId": org, "projectId": projID, "slug": slug, "title": slug, "budgetCents": 1000, "status": "running", "priority": priority}
}

// addItem adds a ready item of the goal; its id is title. A retry
// carries the evaluation of its last attempt.
func (p *priorityFake) addItem(org, goal, title string, seq int, retry bool) {
	row := map[string]any{"id": title, "goalId": goal, "orgId": org, "seq": seq, "title": title,
		"repoId": repoID, "baseRef": "main", "status": "ready", "dependsOn": []string{}}
	if retry {
		row["attempts"] = 1
		row["evaluation"] = "the test does not fail"
	}
	p.WorkItems[title] = row
}

// tick runs one tick with that capacity and returns the items claimed,
// in the order they were claimed.
func (p *priorityFake) tick(capacity int) []string {
	p.t.Helper()
	gw := gateway.New(p.URL(), "rs_test")
	loop := &Loop{GW: gw, Cfg: &Config{Slug: "box", Capacity: capacity}, Scopes: p.scopes, Interval: time.Hour, Harness: "claude-code",
		Dispatch: func(context.Context, gateway.WorkItem) {}}
	before := len(p.CallsNamed("claimWorkItem"))
	loop.Tick(context.Background())
	loop.wg.Wait()
	var claimed []string
	for _, c := range p.CallsNamed("claimWorkItem")[before:] {
		claimed = append(claimed, fmt.Sprint(c.Input["aggregateId"]))
	}
	return claimed
}

// Capacity 1: an urgent goal's item in globex goes before an older item
// of a normal goal in acme, though acme is listed first.
func TestLoopClaimsAnUrgentGoalFirstAcrossOrgs(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addGoal(org2, "fire", "urgent")
	p.addItem(orgID, "steady", "acme-old", 1, false)
	p.addItem(org2, "fire", "globex-urgent", 7, false)
	if got := p.tick(1); strings.Join(got, ",") != "globex-urgent" {
		t.Fatalf("claimed %v, want the urgent item", got)
	}
	if p.Item("acme-old")["status"] != "ready" || p.Item("globex-urgent")["nodeId"] != p.scopes[1].NodeID {
		t.Fatalf("acme-old %v; globex-urgent claimed as %v", p.Item("acme-old")["status"], p.Item("globex-urgent")["nodeId"])
	}
	if got := p.tick(1); strings.Join(got, ",") != "acme-old" {
		t.Fatalf("second tick claimed %v", got)
	}
}

// Empty priority is normal: it ties with normal and breaks on seq, and
// low goes after it.
func TestLoopReadsEmptyPriorityAsNormal(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "unset", "")
	p.addGoal(org2, "normal", "normal")
	p.addGoal(orgID, "low", "low")
	p.addItem(orgID, "low", "low-1", 1, false)
	p.addItem(org2, "normal", "normal-3", 3, false)
	p.addItem(orgID, "unset", "unset-2", 2, false)
	p.addItem(org2, "normal", "normal-4", 4, false)
	if got := strings.Join(p.tick(4), ","); got != "unset-2,normal-3,normal-4,low-1" {
		t.Fatalf("claimed %s", got)
	}
}

// urgent > high > normal > low, whatever the seq and org; within a
// priority a fresh item goes before a retry, then seq.
func TestLoopClaimsInPriorityThenFreshThenSeq(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "low", "low")
	p.addGoal(org2, "normal", "normal")
	p.addGoal(orgID, "high", "high")
	p.addGoal(org2, "urgent", "urgent")
	p.addItem(orgID, "low", "low-1", 1, false)
	p.addItem(org2, "normal", "normal-2", 2, false)
	p.addItem(orgID, "high", "high-retry-3", 3, true)
	p.addItem(orgID, "high", "high-5", 5, false)
	p.addItem(orgID, "high", "high-4", 4, false)
	p.addItem(org2, "urgent", "urgent-9", 9, false)
	want := "urgent-9,high-4,high-5,high-retry-3,normal-2,low-1"
	if got := strings.Join(p.tick(6), ","); got != want {
		t.Fatalf("claimed %s, want %s", got, want)
	}
}

// The goals are read once per org per tick however many items are
// ready, and not at all for an org with nothing ready.
func TestLoopReadsGoalsOncePerOrgPerTick(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "a", "normal")
	p.addGoal(orgID, "b", "high")
	for i := 1; i <= 4; i++ {
		p.addItem(orgID, []string{"a", "b"}[i%2], fmt.Sprintf("item-%d", i), i, false)
	}
	if got := p.tick(2); len(got) != 2 || got[0] != "item-1" || got[1] != "item-3" {
		t.Fatalf("claimed %v", got)
	}
	reads := p.ReadsNamed("goalIndexs")
	if len(reads) != 1 || reads[0].Input["org_id"] != orgID {
		t.Fatalf("goal reads: %+v", reads)
	}
	p.addGoal(org2, "c", "low")
	p.addItem(org2, "c", "globex-1", 1, false)
	p.addItem(org2, "c", "globex-2", 2, false)
	p.tick(1)
	byOrg := map[any]int{}
	for _, r := range p.ReadsNamed("goalIndexs") {
		byOrg[r.Input["org_id"]]++
	}
	if byOrg[orgID] != 2 || byOrg[org2] != 1 {
		t.Fatalf("goal reads by org over two ticks: %v", byOrg)
	}
}

// A goal list that fails leaves the org's items at normal: still
// claimed, after the other org's high item and before its low one.
func TestLoopClaimsAsNormalWhenGoalsFail(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "fire", "urgent")
	p.addGoal(org2, "g", "")
	p.addItem(orgID, "fire", "acme-urgent", 5, false)
	p.addItem(org2, "g", "globex-1", 1, false)
	p.FailReads["goalIndexs"] = "gateway unavailable"
	if got := strings.Join(p.tick(2), ","); got != "globex-1,acme-urgent" {
		t.Fatalf("claimed %s: unread priorities are normal, then seq", got)
	}
}

// claimOrder: unknown and empty priorities rank as normal, a retry
// goes after a fresh item of the same rank, then seq.
func TestClaimOrder(t *testing.T) {
	items := []readyItem{
		{item: gateway.WorkItem{ID: "low", Seq: 1}, rank: gateway.PriorityRank("low")},
		{item: gateway.WorkItem{ID: "unknown", Seq: 3}, rank: gateway.PriorityRank("")},
		{item: gateway.WorkItem{ID: "retry", Seq: 2, Evaluation: "no"}, rank: gateway.PriorityRank("normal")},
		{item: gateway.WorkItem{ID: "fresh", Seq: 4}, rank: gateway.PriorityRank("bogus")},
	}
	claimOrder(items)
	var got []string
	for _, r := range items {
		got = append(got, r.item.ID)
	}
	if strings.Join(got, ",") != "unknown,fresh,retry,low" {
		t.Fatalf("order %v", got)
	}
}
