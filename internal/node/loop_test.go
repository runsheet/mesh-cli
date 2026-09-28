package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
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
	// each item's own GitHub token, an hour to live
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	f.GitHubTokens[item1] = map[string]any{"token": "ghs_item1", "expiresAt": exp, "repo": "acme/widgets"}
	f.GitHubTokens[item2] = map[string]any{"token": "ghs_item2", "expiresAt": exp, "repo": "acme/widgets"}
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
	fetched  []string
	// commits are "<msg> [<paths>]" for each Commit
	commits []string
	// pushedHead and pushedErr are what PushedHead answers.
	pushedHead string
	pushedErr  error
	// events are Authorize (with its token), Prepare, Diff and Push in
	// the order they came
	events []string
}

func (w *fakeWS) Authorize(_ context.Context, spec WorkspaceSpec, tok *gateway.GitHubToken) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, "authorize "+tok.Token)
	return nil
}

func (w *fakeWS) Events() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.events...)
}

func (w *fakeWS) Prepare(_ context.Context, spec WorkspaceSpec) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prepared = append(w.prepared, spec)
	w.events = append(w.events, "prepare")
	dir := spec.Dir()
	return dir, os.MkdirAll(dir, 0o755)
}

func (w *fakeWS) Commit(_ context.Context, dir string, paths []string, msg string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commits = append(w.commits, msg+" ["+strings.Join(paths, ",")+"]")
	w.events = append(w.events, "commit")
	return nil
}

func (w *fakeWS) Push(_ context.Context, dir, branch string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pushed = append(w.pushed, branch)
	w.events = append(w.events, "push")
	return "abc123def456", nil
}

// PushedHead reads back what the session pushed: pushedHead when set,
// else the error pushedErr.
func (w *fakeWS) PushedHead(_ context.Context, dir, branch, base string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, "pushed-head "+branch)
	if w.pushedErr != nil {
		return "", w.pushedErr
	}
	return w.pushedHead, nil
}

func (w *fakeWS) Fetch(_ context.Context, repo, dir string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fetched = append(w.fetched, repo+" → "+dir)
	return os.MkdirAll(dir, 0o755)
}

// assertNoGoalDoc: an item's own branch carries no goal-doc churn — no
// specs/goal-*.md in any workspace the runner prepared, and no commit.
func assertNoGoalDoc(t *testing.T, ws *fakeWS) {
	t.Helper()
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if len(ws.commits) != 0 {
		t.Fatalf("the runner committed on an item's branch: %v", ws.commits)
	}
	for _, spec := range ws.prepared {
		if docs, _ := filepath.Glob(filepath.Join(spec.Dir(), "specs", "goal-*.md")); len(docs) != 0 {
			t.Fatalf("an item's branch must carry no goal doc: %v", docs)
		}
	}
}

func (w *fakeWS) Diff(_ context.Context, dir, base string) (string, error) {
	w.mu.Lock()
	w.events = append(w.events, "diff")
	w.mu.Unlock()
	return "+++ b/thing_test.go\n+func TestThing(t *testing.T) {}", nil
}

type fakeHarness struct {
	mu   sync.Mutex
	jobs []harness.Job
	fail bool
	// text is the final message when set (an evaluator's verdict).
	text string
	// texts, when set, are the final messages of the next runs in turn,
	// before text.
	texts []string
	// toolCalls are the MCP calls by server a run reports; tools, when
	// set, each server's calls, errors and wall time (Result.Tools).
	toolCalls map[string]int
	tools     map[string]*harness.ToolStat
	// turns, inputPerTurnMean, inputPerTurnMax and largestToolResults,
	// when turns is set, are the run's per-turn input and largest tool
	// results (mesh-spends-less-per-item), same shape as a real stream's.
	turns                             int
	inputPerTurnMean, inputPerTurnMax int64
	largestToolResults                []harness.ToolResultSize
}

func (h *fakeHarness) Name() string { return "claude-code" }
func (h *fakeHarness) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	h.jobs = append(h.jobs, job)
	text := "Implemented and tested."
	if h.text != "" {
		text = h.text
	}
	if len(h.texts) > 0 {
		text, h.texts = h.texts[0], h.texts[1:]
	}
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
	var ids map[string][]string
	for server, n := range h.toolCalls {
		if ids == nil {
			ids = map[string][]string{}
		}
		for i := 0; i < n; i++ {
			ids[server] = append(ids[server], fmt.Sprintf("toolu_%s_%d", server, i+1))
		}
	}
	res := harness.Result{Success: true, Text: text, CostUSD: 1.25, SessionID: "s-1", ToolCalls: h.toolCalls, ToolCallIDs: ids, Tools: h.tools}
	if h.turns > 0 {
		res.Turns, res.InputPerTurnMean, res.InputPerTurnMax, res.LargestToolResults = h.turns, h.inputPerTurnMean, h.inputPerTurnMax, h.largestToolResults
	}
	return res, nil
}

func TestLoopClaimsRunsAndFinishes(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(2)}
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
	// no MCP on this runner: no context rule in the prompt
	if strings.Contains(h.jobs[0].Prompt, "mesh_document") || h.jobs[0].MCPServers != nil {
		t.Fatal("no MCP server was given, none should be promised")
	}
	// no goal document on the item's branch: not committed, not written
	assertNoGoalDoc(t, ws)
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
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
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
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	if err := gw.ClaimWorkItem(context.Background(), item1, cfg.NodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	ws := &fakeWS{root: t.TempDir()}
	runner := &Runner{GW: gw, Harness: &fakeHarness{fail: true}, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour}
	var item gateway.WorkItem
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Title = item1, orgID, goalID, repoID, 1, "Write the failing test"
	if err := runner.RunItem(context.Background(), item); err == nil {
		t.Fatal("expected an error")
	}
	if row := f.Item(item1); row["status"] != "failed" || !strings.Contains(row["lastError"].(string), "could not") {
		t.Fatalf("row: %v", row)
	}
	if len(ws.pushed) != 0 {
		t.Fatal("a failed item must not be pushed")
	}
	assertNoGoalDoc(t, ws)
	// the goal's remaining budget bounds the session: (1000-100)/100 = 9
	if b := runner.Budget(&gateway.Goal{BudgetCents: 1000, SpentCents: 100}, "build", ""); b != 9 {
		t.Fatalf("budget = %v", b)
	}
	runner.MaxBudgetUSD = 2
	if b := runner.Budget(&gateway.Goal{BudgetCents: 1000, SpentCents: 100}, "build", ""); b != 2 {
		t.Fatalf("capped budget = %v", b)
	}
}

// A goal spanning two repos ends with an item in the second: its doc is
// written once, final, on the default branch of the project's first repo
// — never on an item's branch, never in the second repo — and a second
// writer finding the doc already final pushes nothing.
func TestRunnerWritesEndedGoalDocOnceInPrimaryRepo(t *testing.T) {
	f := seed(t)
	const repoID2 = "cccccccc-2222-4222-8222-cccccccccccc"
	f.Repos[repoID2] = map[string]any{"id": repoID2, "accountId": acctID, "name": "gadgets", "defaultBranch": "trunk"}
	f.Projects[projID]["repoIds"] = []string{repoID, repoID2}
	f.WorkItems[item1]["status"] = "merged"
	f.WorkItems[item2]["repoId"], f.WorkItems[item2]["status"], f.WorkItems[item2]["baseRef"] = repoID2, "ready", "trunk"
	f.GitHubTokens[item2]["repo"] = "acme/gadgets"
	// the platform: item 2's finish is the goal's last — it merges and
	// the goal is done with its spend final
	f.After = func(name string, in map[string]any) {
		if name == "finishWorkItem" && in["aggregateId"] == item2 {
			f.WorkItems[item2]["status"] = "merged"
			f.Goals[goalID]["status"] = "done"
			f.Goals[goalID]["spentCents"] = 425
		}
	}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	ws := &fakeWS{root: t.TempDir()}
	runner := &Runner{GW: gw, Harness: &fakeHarness{}, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour}
	ctx := context.Background()
	if err := gw.ClaimWorkItem(ctx, item2, cfg.NodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	var item gateway.WorkItem
	item.ID, item.GoalID, item.OrgID, item.RepoID, item.Seq, item.BaseRef = item2, goalID, orgID, repoID2, 2, "trunk"
	if err := runner.RunItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	// another item's finish racing this one sees the same ended goal
	if err := RecordEndedGoal(ctx, gw, ws, ws.root, "acme", goalID, nil); err != nil {
		t.Fatal(err)
	}

	// the items ran in gadgets on their own branches; the doc was
	// prepared, committed and pushed once, in widgets on main
	var docSpecs []WorkspaceSpec
	for _, spec := range ws.prepared {
		switch spec.Repo {
		case "acme/gadgets":
			if !strings.HasPrefix(spec.Branch, "mesh/ship-it-") {
				t.Fatalf("gadgets prepared off an item branch: %+v", spec)
			}
			if docs, _ := filepath.Glob(filepath.Join(spec.Dir(), "specs", "goal-*.md")); len(docs) != 0 {
				t.Fatalf("the second repo must carry no goal doc: %v", docs)
			}
		case "acme/widgets":
			docSpecs = append(docSpecs, spec)
		default:
			t.Fatalf("prepared: %+v", spec)
		}
	}
	// the second writer prepares the primary again, but the doc is
	// already final: nothing to commit, nothing to push
	if len(docSpecs) != 2 || docSpecs[0].Branch != "main" || docSpecs[0].BaseRef != "main" {
		t.Fatalf("primary repo prepared: %+v", docSpecs)
	}
	var docCommits []string
	for _, c := range ws.commits {
		if strings.Contains(c, "docs(mesh): goal") {
			docCommits = append(docCommits, c)
		}
	}
	if len(docCommits) != 1 || docCommits[0] != "docs(mesh): goal ship-it — finished [specs/goal-ship-it.md]" {
		t.Fatalf("doc commits: %v", ws.commits)
	}
	if strings.Join(ws.pushed, ",") != "mesh/ship-it-2,main" {
		t.Fatalf("pushed: %v", ws.pushed)
	}
	if len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatal("no item fails")
	}

	// the final state: approved, with a summary of items merged and spend
	body, err := os.ReadFile(GoalDocPath(docSpecs[0].Dir(), "ship-it"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(body)
	for _, want := range []string{"\nstatus: approved\n", "\nsummary: 2 of 2 items merged · $4.25 spent\n", "\nparent: prd\n", "Mesh goal `ship-it`, done."} {
		if !strings.Contains(doc, want) {
			t.Fatalf("doc lacks %q:\n%s", want, doc)
		}
	}
}

func TestRunnerLeavesItemOnShutdown(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
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
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq = item1, orgID, goalID, repoID, 1
	if err := runner.RunItem(ctx, item); err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	if row := f.Item(item1); row["status"] == "failed" {
		t.Fatal("our own shutdown must not fail the item")
	}
	assertNoGoalDoc(t, ws)
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

	cfg := &Config{Slug: "box", Name: "Box", Capacity: Fixed(1)}
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
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
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
		item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Title = id, orgID, goalID, repoID, seq+1, "Write the failing test"
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
	assertNoGoalDoc(t, ws)
	for i, want := range []string{"Bearer machine-token-1", "Bearer machine-token-2"} {
		srv, _ := h.jobs[i].MCPServers["prologue"].(map[string]any)
		headers, _ := srv["headers"].(map[string]string)
		if headers["Authorization"] != want {
			t.Fatalf("job %d Authorization = %q, want %q (MCP must not be the map from node start)", i, headers["Authorization"], want)
		}
	}
	// the prompt promises mesh-context because MCPFunc gave a server
	if !strings.Contains(h.jobs[0].Prompt, "mesh_document") {
		t.Fatal("a session with mesh-context must be told about it")
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
	scopes, err := ResolveScopes(context.Background(), gw, &Config{Slug: "box", Name: "Box", Capacity: Fixed(1)})
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

// addPlanning adds a goal of that priority to the org, in planning.
func (p *priorityFake) addPlanning(org, slug, priority string) {
	p.addGoal(org, slug, priority)
	p.Goals[slug]["status"] = "planning"
}

// addHeld adds an item of the goal that this node finished and holds
// for evaluation; its id is title.
func (p *priorityFake) addHeld(org, goal, title string, seq int) {
	p.addItem(org, goal, title, seq, false)
	node := p.scopes[0].NodeID
	if org == org2 {
		node = p.scopes[1].NodeID
	}
	p.WorkItems[title]["status"], p.WorkItems[title]["nodeId"] = "evaluating", node
}

// loop is a node over both orgs with that capacity. It plans and
// evaluates too, doing nothing, and logs into the returned buffer: the
// "planning goal" and "evaluating item" lines are written as each
// session starts, in the order they start.
func (p *priorityFake) loop(capacity int) (*Loop, *strings.Builder) {
	var logs strings.Builder
	return &Loop{GW: gateway.New(p.URL(), "rs_test"), Cfg: &Config{Slug: "box", Capacity: Fixed(capacity)}, Scopes: p.scopes,
		Interval: time.Hour, Harness: "claude-code", Log: log.New(&logs, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Evaluate: func(context.Context, gateway.WorkItem) {},
		Plan:     func(context.Context, gateway.Goal, Scope) {},
		Research: func(context.Context, gateway.Direction, gateway.Project, Scope) {},
	}, &logs
}

// started lists what the log says was started with that prefix
// ("planning goal", "evaluating item"), in order: the quoted name.
func started(logs, prefix string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if i := strings.Index(line, prefix); i >= 0 {
			rest := line[i+len(prefix):]
			if q := strings.Index(rest, `"`); q >= 0 {
				out = append(out, strings.Trim(rest[q:], `"`))
			}
		}
	}
	return out
}

// tick runs one tick with that capacity and returns the items claimed,
// in the order they were claimed.
func (p *priorityFake) tick(capacity int) []string {
	p.t.Helper()
	loop := &Loop{GW: gateway.New(p.URL(), "rs_test"), Cfg: &Config{Slug: "box", Capacity: Fixed(capacity)}, Scopes: p.scopes, Interval: time.Hour, Harness: "claude-code",
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

// A planner for an urgent goal in globex is claimed and started before
// one for a normal goal in acme, though acme is listed first.
func TestLoopPlansAnUrgentGoalFirstAcrossOrgs(t *testing.T) {
	p := newPriorityFake(t)
	p.addPlanning(orgID, "steady", "normal")
	p.addPlanning(org2, "fire", "urgent")
	loop, logs := p.loop(1)
	loop.Tick(context.Background())
	loop.wg.Wait()
	var claims []string
	for _, c := range p.CallsNamed("claimPlan") {
		claims = append(claims, fmt.Sprint(c.Input["aggregateId"]))
	}
	if strings.Join(claims, ",") != "fire,steady" {
		t.Fatalf("plans claimed %v, want fire first", claims)
	}
	if got := strings.Join(started(logs.String(), "planning goal"), ","); got != "fire,steady" {
		t.Fatalf("planners started %s\n%s", got, logs)
	}
}

// An evaluation of an urgent goal's item in globex starts before one of
// a normal goal's older item in acme.
func TestLoopEvaluatesAnUrgentGoalFirstAcrossOrgs(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	p.addGoal(org2, "fire", "urgent")
	p.addHeld(orgID, "steady", "acme-old", 1)
	p.addHeld(org2, "fire", "globex-urgent", 7)
	loop, logs := p.loop(1)
	loop.Tick(context.Background())
	loop.wg.Wait()
	if got := strings.Join(started(logs.String(), "evaluating item"), ","); got != "globex-urgent,acme-old" {
		t.Fatalf("evaluations started %s\n%s", got, logs)
	}
}

// Empty priority is normal for plans and evaluations too: after high,
// before low; evaluations of one rank go by seq.
func TestLoopPlansAndEvaluatesEmptyPriorityAsNormal(t *testing.T) {
	p := newPriorityFake(t)
	p.addPlanning(orgID, "low", "low")
	p.addPlanning(orgID, "unset", "")
	p.addPlanning(org2, "high", "high")
	p.addGoal(orgID, "g-low", "low")
	p.addGoal(orgID, "g-unset", "")
	p.addGoal(org2, "g-normal", "normal")
	p.addGoal(org2, "g-high", "high")
	p.addHeld(orgID, "g-low", "low-1", 1)
	p.addHeld(orgID, "g-unset", "unset-3", 3)
	p.addHeld(org2, "g-normal", "normal-2", 2)
	p.addHeld(org2, "g-high", "high-9", 9)
	loop, logs := p.loop(1)
	loop.Tick(context.Background())
	loop.wg.Wait()
	if got := strings.Join(started(logs.String(), "planning goal"), ","); got != "high,unset,low" {
		t.Fatalf("planners started %s", got)
	}
	if got := strings.Join(started(logs.String(), "evaluating item"), ","); got != "high-9,normal-2,unset-3,low-1" {
		t.Fatalf("evaluations started %s", got)
	}
}

// A failed goals read leaves an org's evaluations at normal: still
// started, ordered by seq among the other normals.
func TestLoopEvaluatesAsNormalWhenGoalsFail(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "fire", "urgent")
	p.addGoal(org2, "g", "")
	p.addHeld(orgID, "fire", "acme-urgent", 5)
	p.addHeld(org2, "g", "globex-1", 1)
	p.FailReads["goalIndexs"] = "gateway unavailable"
	loop, logs := p.loop(1)
	loop.Tick(context.Background())
	loop.wg.Wait()
	if got := strings.Join(started(logs.String(), "evaluating item"), ","); got != "globex-1,acme-urgent" {
		t.Fatalf("evaluations started %s", got)
	}
}

// A whole tick — ready items, goals in planning and held evaluations in
// both orgs, and the director looking for room — reads each org's goals
// exactly once.
func TestLoopTickReadsGoalsOncePerOrg(t *testing.T) {
	p := newPriorityFake(t)
	for _, org := range []string{orgID, org2} {
		g, plan := "run-"+org[:4], "plan-"+org[:4]
		p.addGoal(org, g, "normal")
		p.addPlanning(org, plan, "high")
		p.addItem(org, g, "ready-"+org[:4], 1, false)
		p.addHeld(org, g, "held-"+org[:4], 2)
	}
	// a live direction in each org, with room, so the director reads goals
	const proj2 = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
	p.Projects[proj2] = map[string]any{"id": proj2, "orgId": org2, "slug": "other", "name": "Other"}
	direction(p.Fake, dirID, projID, "nightly", "Tidy up.", 5)
	direction(p.Fake, "eeeeeeee-2222-4222-8222-eeeeeeeeeeee", proj2, "nightly", "Tidy up.", 5)["orgId"] = org2
	loop, logs := p.loop(2)
	loop.Tick(context.Background())
	loop.wg.Wait()
	if n := len(started(logs.String(), "planning goal")); n != 2 {
		t.Fatalf("planners started %d\n%s", n, logs)
	}
	if n := len(started(logs.String(), "evaluating item")); n != 2 {
		t.Fatalf("evaluations started %d\n%s", n, logs)
	}
	if n := len(p.CallsNamed("claimWorkItem")); n != 2 {
		t.Fatalf("claimed %d items", n)
	}
	if n := len(p.CallsNamed("claimDirectionResearch")); n != 2 {
		t.Fatalf("research claimed %d times\n%s", n, logs)
	}
	byOrg := map[any]int{}
	for _, r := range p.ReadsNamed("goalIndexs") {
		byOrg[r.Input["org_id"]]++
	}
	if len(byOrg) != 2 || byOrg[orgID] != 1 || byOrg[org2] != 1 {
		t.Fatalf("goal reads by org in one tick: %v", byOrg)
	}
}

// liveLoop is a node over acme only whose Reload reads node.yaml from a
// fresh HOME and MyOrgs from the fake, registering in a new org through
// the returned list. Items finish as soon as they are dispatched.
func (p *priorityFake) liveLoop(t *testing.T, yaml string) (*Loop, *[]string, *strings.Builder) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeNodeYAML(t, yaml)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(p.URL(), "rs_test")
	var registered []string
	var logs strings.Builder
	start := p.scopes[:1]
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: start, Interval: time.Hour, Harness: "claude-code", Log: log.New(&logs, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {}}
	loop.Reload = Reloader(gw, cfg, start, nil, func(_ context.Context, sc Scope) error {
		registered = append(registered, sc.Login)
		return nil
	})
	return loop, &registered, &logs
}

func writeNodeYAML(t *testing.T, body string) {
	t.Helper()
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// claimedBy runs one tick and returns the items it claimed.
func (p *priorityFake) claimedBy(loop *Loop) []string {
	p.t.Helper()
	before := len(p.CallsNamed("claimWorkItem"))
	loop.Tick(context.Background())
	loop.wg.Wait()
	var claimed []string
	for _, c := range p.CallsNamed("claimWorkItem")[before:] {
		claimed = append(claimed, fmt.Sprint(c.Input["aggregateId"]))
	}
	return claimed
}

// Capacity is read from node.yaml every tick: raised from 1 to 2, the
// next tick claims two with no restart.
func TestLoopTakesCapacityFromNodeYAMLEveryTick(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	for i, id := range []string{"a1", "a2", "a3"} {
		p.addItem(orgID, "steady", id, i+1, false)
	}
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 1\n")
	if got := p.claimedBy(loop); len(got) != 1 {
		t.Fatalf("first tick claimed %v, want one", got)
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 2\n")
	if got := p.claimedBy(loop); strings.Join(got, ",") != "a2,a3" {
		t.Fatalf("second tick claimed %v, want a2,a3", got)
	}
	if !strings.Contains(logs.String(), "capacity 1 → 2") {
		t.Fatalf("the change is not logged:\n%s", logs)
	}
}

// An org that appears in MyOrgs between two ticks is registered in and
// its ready items are claimed on the next tick.
func TestLoopServesANewOrgOnTheNextTick(t *testing.T) {
	p := newPriorityFake(t)
	globex := p.Orgs[1]
	p.Orgs = p.Orgs[:1]
	p.addGoal(org2, "fire", "normal")
	p.addItem(org2, "fire", "globex-1", 1, false)
	loop, registered, logs := p.liveLoop(t, "slug: box\nname: Box\ncapacity: 2\n")
	if got := p.claimedBy(loop); len(got) != 0 {
		t.Fatalf("claimed %v from an org the node does not serve", got)
	}
	p.Orgs = append(p.Orgs, globex)
	if got := p.claimedBy(loop); strings.Join(got, ",") != "globex-1" {
		t.Fatalf("second tick claimed %v, want globex-1", got)
	}
	if strings.Join(*registered, ",") != "globex" || p.Item("globex-1")["nodeId"] != p.scopes[1].NodeID {
		t.Fatalf("registered in %v; globex-1 claimed as %v", *registered, p.Item("globex-1")["nodeId"])
	}
	if got := loop.CurrentScopes().Logins(); strings.Join(got, ",") != "acme,globex" {
		t.Fatalf("scopes = %v", got)
	}
	if !strings.Contains(logs.String(), "orgs acme → acme, globex") {
		t.Fatalf("the change is not logged:\n%s", logs)
	}
	// one left, and one outside the node.yaml restriction, are dropped
	p.Orgs = p.Orgs[1:]
	p.claimedBy(loop)
	if got := loop.CurrentScopes().Logins(); strings.Join(got, ",") != "globex" {
		t.Fatalf("after acme left, scopes = %v", got)
	}
	p.Orgs = append(p.Orgs, map[string]any{"id": orgID, "login": "acme", "accountId": acctID})
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 2\n")
	p.claimedBy(loop)
	if got := loop.CurrentScopes().Logins(); strings.Join(got, ",") != "acme" {
		t.Fatalf("restricted to acme, scopes = %v", got)
	}
}

// A node.yaml that cannot be read keeps the last good capacity.
func TestLoopKeepsTheLastGoodCapacity(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "steady", "normal")
	for i, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		p.addItem(orgID, "steady", id, i+1, false)
	}
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 2\n")
	if got := p.claimedBy(loop); len(got) != 2 {
		t.Fatalf("first tick claimed %v, want two", got)
	}
	writeNodeYAML(t, "slug: box\ncapacity: [not a number\n")
	if got := p.claimedBy(loop); len(got) != 2 || loop.Capacity() != 2 {
		t.Fatalf("with node.yaml broken, claimed %v at capacity %d; want two at 2", got, loop.Capacity())
	}
	if !strings.Contains(logs.String(), "reload config: node.yaml (keeping the last good capacity and limits)") {
		t.Fatalf("the unreadable file is not logged:\n%s", logs)
	}
	if got := loop.CurrentScopes().Logins(); strings.Join(got, ",") != "acme" {
		t.Fatalf("scopes = %v", got)
	}
}

// register registers the node in both orgs, so its heartbeats land.
func (p *priorityFake) register() {
	p.t.Helper()
	gw := gateway.New(p.URL(), "rs_test")
	for _, sc := range p.scopes {
		if err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: sc.NodeID, OrgID: sc.ID, OwnerID: p.UserID, Name: "Box", Slug: "box"}); err != nil {
			p.t.Fatal(err)
		}
	}
}

// drainLoop is a node over both orgs whose builds block until release
// is closed.
func (p *priorityFake) drainLoop(capacity int) (*Loop, chan struct{}) {
	release := make(chan struct{})
	loop, _ := p.loop(capacity)
	loop.Dispatch = func(ctx context.Context, _ gateway.WorkItem) {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	return loop, release
}

// DrainAndWait waits for the running item and returns once it ends.
func TestLoopDrainAndWaitWaitsForRunning(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	p.addItem(orgID, "g", "long-one", 1, false)
	loop, release := p.drainLoop(1)
	loop.Tick(context.Background())
	if loop.Running() != 1 {
		t.Fatalf("running = %d, want 1", loop.Running())
	}
	done := make(chan error, 1)
	go func() { done <- loop.DrainAndWait(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("DrainAndWait returned (%v) with an item running", err)
	case <-time.After(200 * time.Millisecond):
	}
	if !loop.Draining() {
		t.Fatal("Draining() = false while draining")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DrainAndWait = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DrainAndWait did not return after the item ended")
	}
	if loop.Running() != 0 {
		t.Fatalf("running = %d after drain", loop.Running())
	}
}

// DrainAndWait gives up with ctx.Err() when ctx is done first.
func TestLoopDrainAndWaitStopsWithContext(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	p.addItem(orgID, "g", "long-one", 1, false)
	loop, release := p.drainLoop(1)
	defer close(release)
	loop.Tick(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.DrainAndWait(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("DrainAndWait = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DrainAndWait did not return on cancel")
	}
	if loop.Running() != 1 {
		t.Fatalf("running = %d, the item must still be running", loop.Running())
	}
}

// Resume makes the next tick claim again.
func TestLoopResumeClaimsAgain(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	p.addItem(orgID, "g", "ready-one", 1, false)
	p.register()
	loop, _ := p.loop(1)
	loop.Drain()
	loop.Tick(context.Background())
	if c := p.CallsNamed("claimWorkItem"); len(c) != 0 {
		t.Fatalf("drained: claimed %+v", c)
	}
	loop.Resume()
	if loop.Draining() {
		t.Fatal("Draining() = true after Resume")
	}
	loop.Tick(context.Background())
	loop.wg.Wait()
	if c := p.CallsNamed("claimWorkItem"); len(c) != 1 || c[0].Input["aggregateId"] != "ready-one" {
		t.Fatalf("resumed: claims %+v", c)
	}
	if hb := p.CallsNamed("heartbeatNode"); hb[len(hb)-1].Input["draining"] != nil {
		t.Fatalf("a resumed node must not report draining: %+v", hb[len(hb)-1].Input)
	}
}

// The heartbeat's extras are sent only when set: a platform that
// does not know them yet still takes an ordinary node's heartbeat.
func TestHeartbeatSendsExtrasOnlyWhenSet(t *testing.T) {
	p := newPriorityFake(t)
	p.register()
	gw := gateway.New(p.URL(), "rs_test")
	node := p.scopes[0].NodeID
	ctx := context.Background()
	calls := []error{
		gw.HeartbeatNode(ctx, node, "v1", 0),
		gw.HeartbeatNode(ctx, node, "v1", 0, gateway.HeartbeatExtras{}),
		gw.HeartbeatNode(ctx, node, "v1", 1, gateway.HeartbeatExtras{Draining: true}),
		gw.HeartbeatNode(ctx, node, "v1", 0, gateway.HeartbeatExtras{UpdateError: "signature does not match"}),
	}
	for i, err := range calls {
		if err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
	}
	hb := p.CallsNamed("heartbeatNode")
	if len(hb) != 4 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	for i, want := range []map[string]any{
		{},
		{},
		{"draining": true},
		{"updateError": "signature does not match"},
	} {
		in := hb[i].Input
		for _, k := range []string{"draining", "updateError"} {
			if got, ok := in[k]; ok != (want[k] != nil) || (ok && got != want[k]) {
				t.Fatalf("heartbeat %d: %s = %v (sent %v), want %v", i, k, got, ok, want[k])
			}
		}
		if in["version"] != "v1" || in["aggregateId"] != node {
			t.Fatalf("heartbeat %d: %+v", i, in)
		}
	}
}

// The heartbeat carries the tool runtimes and agent adapters the node
// found, probed every tick: one installed while the node runs is on the
// next heartbeat without a restart (trd §1 Node).
func TestHeartbeatCarriesProbedRuntimes(t *testing.T) {
	p := newPriorityFake(t)
	p.register()
	loop, logs := p.loop(1)
	onPath := map[string]bool{"npx": true, "claude": true}
	loop.LookPath = func(file string) (string, error) {
		if onPath[file] {
			return "/usr/bin/" + file, nil
		}
		return "", errors.New("not found")
	}
	ctx := context.Background()
	loop.Tick(ctx)
	onPath["docker"], onPath["codex"] = true, true
	loop.Tick(ctx)
	// one heartbeat per org served, per tick
	orgs := len(p.scopes)
	hb := p.CallsNamed("heartbeatNode")
	if len(hb) != 2*orgs {
		t.Fatalf("heartbeats: %+v", hb)
	}
	for i := range hb {
		want := []struct{ runtimes, adapters []any }{
			{[]any{"npx"}, []any{"claude-code"}},
			{[]any{"npx", "docker"}, []any{"claude-code", "codex"}},
		}[i/orgs]
		if got := hb[i].Input["runtimes"]; !reflect.DeepEqual(got, want.runtimes) {
			t.Fatalf("heartbeat %d: runtimes = %#v, want %v", i, got, want.runtimes)
		}
		if got := hb[i].Input["adapters"]; !reflect.DeepEqual(got, want.adapters) {
			t.Fatalf("heartbeat %d: adapters = %#v, want %v", i, got, want.adapters)
		}
	}
	if r, a := loop.Runtimes(); !reflect.DeepEqual(r, []string{"npx", "docker"}) || !reflect.DeepEqual(a, []string{"claude-code", "codex"}) {
		t.Fatalf("Runtimes() = %v, %v", r, a)
	}
	if n := strings.Count(logs.String(), "tools: runtimes"); n != 2 {
		t.Fatalf("logged the probe %d times, want once per change:\n%s", n, logs)
	}

	// without a LookPath the node probes nothing and sends neither
	bare, _ := p.loop(1)
	bare.Tick(ctx)
	last := p.CallsNamed("heartbeatNode")[2*orgs].Input
	for _, k := range []string{"runtimes", "adapters"} {
		if v, ok := last[k]; ok {
			t.Fatalf("a node that probed nothing sent %s = %#v", k, v)
		}
	}
}

// Resume ends a DrainAndWait that is still waiting, with an error.
func TestLoopResumeEndsDrainAndWait(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	p.addItem(orgID, "g", "long-one", 1, false)
	loop, release := p.drainLoop(1)
	defer close(release)
	loop.Tick(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.DrainAndWait(context.Background()) }()
	for !loop.Draining() {
		time.Sleep(time.Millisecond)
	}
	loop.Resume()
	select {
	case err := <-done:
		if !errors.Is(err, errResumed) {
			t.Fatalf("DrainAndWait = %v, want errResumed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DrainAndWait did not return on Resume")
	}
}

// A draining node claims nothing on a tick: no ready item, no plan, no
// research, no evaluation — but it still heartbeats.
func TestDrainingLoopClaimsNothing(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	p.addItem(orgID, "g", "ready-1", 1, false)
	p.addItem(org2, "g", "ready-2", 2, false)
	p.addPlanning(org2, "plan-me", "urgent")
	p.addHeld(orgID, "g", "held-3", 3)
	loop, logs := p.loop(4)
	for _, sc := range p.scopes {
		if err := loop.GW.(*gateway.Client).RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: sc.NodeID, OrgID: sc.ID, Name: "Box", Slug: "box", Capacity: 4}); err != nil {
			t.Fatal(err)
		}
	}
	loop.Drain()
	beats := len(p.CallsNamed("heartbeatNode"))
	loop.Tick(context.Background())
	loop.wg.Wait()
	for _, name := range []string{"claimWorkItem", "claimPlan", "claimDirectionResearch"} {
		if calls := p.CallsNamed(name); len(calls) != 0 {
			t.Fatalf("a draining node called %s: %+v", name, calls)
		}
	}
	if got := started(logs.String(), "evaluating item"); len(got) != 0 {
		t.Fatalf("a draining node started evaluations %v", got)
	}
	hb := p.CallsNamed("heartbeatNode")
	if got := len(hb) - beats; got != 2 {
		t.Fatalf("a draining node heartbeat %d times over two orgs\n%s", got, logs)
	}
	for _, c := range hb[beats:] {
		if c.Input["draining"] != true {
			t.Fatalf("a draining node's heartbeat does not say so: %+v", c.Input)
		}
	}
	if p.Item("ready-1")["status"] != "ready" || !loop.Draining() {
		t.Fatalf("ready-1 %v, draining %v", p.Item("ready-1")["status"], loop.Draining())
	}
	if loop.Adopt(context.Background(), gateway.WorkItem{ID: "held-elsewhere"}) {
		t.Fatal("a draining node adopted a held item")
	}
}

// runUntilDrained starts Run on a draining loop and checks it is still
// running while release has not been called, then that it returns nil
// once it has.
func runUntilDrained(t *testing.T, loop *Loop, release func()) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- loop.Run(context.Background()) }()
	loop.Drain()
	select {
	case err := <-done:
		t.Fatalf("Run returned %v with a session still running", err)
	case <-time.After(150 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a drained Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the last session ended")
	}
}

// A draining loop's Run waits for its running item, then returns nil.
func TestDrainingRunWaitsForTheRunningItem(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	p.addItem(orgID, "g", "long", 1, false)
	p.addItem(orgID, "g", "next", 2, false)
	loop, _ := p.loop(1)
	hold := make(chan struct{})
	loop.Dispatch = func(context.Context, gateway.WorkItem) { <-hold }
	loop.Tick(context.Background())
	if loop.Running() != 1 {
		t.Fatalf("running %d before the drain", loop.Running())
	}
	runUntilDrained(t, loop, func() { close(hold) })
	if loop.Running() != 0 || p.Item("next")["status"] != "ready" {
		t.Fatalf("running %d, next %v", loop.Running(), p.Item("next")["status"])
	}
}

// A planner holds the exit too, though it does not count against
// capacity.
func TestDrainingRunWaitsForAPlanner(t *testing.T) {
	p := newPriorityFake(t)
	p.addPlanning(orgID, "plan-me", "normal")
	loop, _ := p.loop(1)
	hold := make(chan struct{})
	loop.Plan = func(context.Context, gateway.Goal, Scope) { <-hold }
	loop.Tick(context.Background())
	if loop.Running() != 0 {
		t.Fatalf("a planner counted as running: %d", loop.Running())
	}
	runUntilDrained(t, loop, func() { close(hold) })
}

// So does an evaluator.
func TestDrainingRunWaitsForAnEvaluator(t *testing.T) {
	p := newPriorityFake(t)
	p.addGoal(orgID, "g", "normal")
	p.addHeld(orgID, "g", "held-1", 1)
	loop, _ := p.loop(1)
	hold := make(chan struct{})
	loop.Evaluate = func(context.Context, gateway.WorkItem) { <-hold }
	loop.Tick(context.Background())
	if loop.Running() != 0 {
		t.Fatalf("an evaluator counted as running: %d", loop.Running())
	}
	runUntilDrained(t, loop, func() { close(hold) })
}

// A drain with nothing running ends Run straight away.
func TestDrainingIdleRunReturnsNil(t *testing.T) {
	p := newPriorityFake(t)
	loop, _ := p.loop(1)
	loop.Drain()
	if err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run = %v", err)
	}
}
