package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// An item this node finished sits in evaluating; the tick hands it to
// the evaluator, whose fresh session sees the diff and the acceptance
// and whose verdict line is recorded. No verdict, nothing recorded.
func TestLoopEvaluatesFinishedItems(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node"}); err != nil {
		t.Fatal(err)
	}
	// item 1 finished here; item 2's evaluation another node has
	// claimed, so the claim is refused and it is not ours to judge
	// (ADR-025)
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	f.WorkItems[item2]["status"] = "evaluating"
	f.WorkItems[item2]["nodeId"] = "33333333-3333-4333-8333-333333333333"
	f.WorkItems[item2]["evaluationNodeId"] = "33333333-3333-4333-8333-333333333333"
	f.WorkItems[item2]["branch"] = "mesh/ship-it-2"

	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "The test exists and fails for the right reason.\n\nVERDICT: PASS"}
	ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MCP: MeshContextHTTP(f.URL(), "rs_test"), MaxBudgetUSD: 2, Log: log.New(os.Stderr, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: ev.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Evaluate: func(ctx context.Context, item gateway.WorkItem) { _ = ev.Evaluate(ctx, item) }}
	// an evaluation takes no slot; none lets a tick claim the item a FAIL readies
	cfg.Capacity = Fixed(0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// the session hands its PASS to the product's tool, which moves the
	// item on; the node closes the job with its notes tail (runsheet ADR 077)
	f.HandedInVerdicts = map[string]map[string]any{item1: {"passed": true, "notes": "The test exists and fails for the right reason."}}
	loop.Tick(ctx)
	waitFor(t, func() bool { return f.Item(item1)["status"] == "review" })
	loop.wg.Wait()

	if in := evaluationClosed(t, f); in["notes"] != "(evaluated on Claude the CLI's default model)" || in["costCents"] != float64(125) {
		t.Fatalf("closed %+v", in)
	}
	if f.Item(item2)["status"] != "evaluating" {
		t.Fatal("another node's item is not ours to evaluate")
	}
	if len(ws.prepared) != 1 || ws.prepared[0].Branch != "mesh/ship-it-1" || ws.prepared[0].BaseRef != "main" {
		t.Fatalf("prepared: %+v", ws.prepared)
	}
	if len(ws.pushed) != 0 {
		t.Fatal("the evaluator changes nothing")
	}
	if len(h.jobs) != 1 || h.jobs[0].Kind != harness.KindEvaluate || h.jobs[0].MaxBudgetUSD != 2 || h.jobs[0].MCPServers["runsheet"] == nil {
		t.Fatalf("job: %+v", h.jobs)
	}
	if _, err := os.Stat(filepath.Join(ws.prepared[0].Dir(), ContextDir, "instructions-acme-widgets-agents.md")); err != nil {
		t.Fatal("the evaluator must have its repo's instructions on disk")
	}
	if _, err := os.Stat(filepath.Join(ws.prepared[0].Dir(), ContextDir, "adr-adr-001.md")); err == nil {
		t.Fatal("no ADR is on disk: it is fetched")
	}
	p := h.jobs[0].Prompt
	for _, want := range []string{"Write the failing test", "the test fails", "go test ./... passes", "+func TestThing", "origin/main...HEAD", "# How this job finishes", "mesh_verdict", "Change nothing",
		".mesh/context/", "instructions-acme-widgets-agents.md", "goes against an ADR", "mesh_document", `project "acme/mesh"`} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	// a second tick finds nothing of ours left to evaluate
	loop.Tick(ctx)
	loop.wg.Wait()
	if len(h.jobs) != 1 {
		t.Fatalf("a reviewed item was evaluated again: %d jobs", len(h.jobs))
	}

	// a failing verdict the session handed in readies it again; a session
	// that handed nothing in is refused at the close and leaves it
	// evaluating
	f.WorkItems[item1]["status"] = "evaluating"
	f.HandedInVerdicts = map[string]map[string]any{item1: {"passed": false, "notes": "The test passes already, so it proves nothing."}}
	loop.Tick(ctx)
	waitFor(t, func() bool { return f.Item(item1)["status"] == "ready" })
	loop.wg.Wait()
	f.WorkItems[item1]["status"] = "evaluating"
	f.HandedInVerdicts = nil
	f.EvaluationNotHandedIn = "finishJob: call mesh_verdict first"
	loop.Tick(ctx)
	loop.wg.Wait()
	if f.Item(item1)["status"] != "evaluating" {
		t.Fatalf("no verdict must leave the item in evaluating: %v", f.Item(item1)["status"])
	}
}

// A retry's build prompt carries the evaluator's notes.
func TestBuildPromptOnRetry(t *testing.T) {
	p := harness.BuildPrompt(harness.PromptInput{GoalTitle: "Ship it", Seq: 1, Title: "T", Attempts: 1, Evaluation: "the test proves nothing"})
	if !strings.Contains(p, "attempt 2") || !strings.Contains(p, "the test proves nothing") {
		t.Fatalf("retry prompt:\n%s", p)
	}
	if strings.Contains(harness.BuildPrompt(harness.PromptInput{GoalTitle: "Ship it", Seq: 1, Title: "T"}), "Previous attempt") {
		t.Fatal("a first attempt has no previous one")
	}
}

// fakeCodexH is a second-opinion harness: its final message, or its
// error, and a plan reading it reports.
type fakeCodexH struct {
	mu    sync.Mutex
	jobs  []harness.Job
	text  string
	err   error
	cost  float64
	rl    *harness.RateLimit
	usage *harness.Usage
	// tools, when set, is each MCP server's calls and errors a run
	// reports (Result.Tools); firstOnly reports them on the first run
	// alone, as a context server that failed once and then answered.
	tools     map[string]*harness.ToolStat
	firstOnly bool
	// sandbox, when set, is Result.SandboxBroken: every command the run
	// made failed on bubblewrap.
	sandbox string
	// workCalls are the mesh-work tool inputs a run reports
	// (Result.WorkCalls, ADR-023).
	workCalls map[string]map[string]any
}

func (h *fakeCodexH) Name() string { return "codex" }
func (h *fakeCodexH) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	h.jobs = append(h.jobs, job)
	tools := h.tools
	if h.firstOnly && len(h.jobs) > 1 {
		tools = nil
	}
	h.mu.Unlock()
	if h.err == nil {
		handInScript(job, nil, h.text, h.workCalls)
	}
	var calls map[string]int
	for server, s := range tools {
		if calls == nil {
			calls = map[string]int{}
		}
		calls[server] = s.Calls
	}
	return harness.Result{Success: h.err == nil, Text: h.text, CostUSD: h.cost, RateLimit: h.rl, Usage: h.usage, Tools: tools, ToolCalls: calls, SandboxBroken: h.sandbox, WorkCalls: h.workCalls}, h.err
}

func (h *fakeCodexH) runs() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.jobs)
}

// secondFixture is an evaluator on item 1 of a goal of that priority,
// with Claude saying first and second as the second opinion, on the one
// issued Codex credential the node knows, plan (oa-1); the second's
// readings land in the returned Limits under codex:plan.
func secondFixture(t *testing.T, priority, first string, second harness.Harness) (*gatewaytest.Fake, *Evaluator, *fakeHarness, *Limits) {
	t.Helper()
	cacheRoot(t)
	f := seed(t)
	f.Goals[goalID]["priority"] = priority
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	// pinned to Claude, so Claude judges first and Codex gives the
	// second opinion
	f.WorkItems[item1]["evaluateModel"] = "claude-sonnet-5"
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	ws := &fakeWS{root: t.TempDir()}
	claude := &fakeHarness{text: first}
	plan := gateway.Credential{ID: "oa-1", Label: "plan", Kind: AccountSubscription, Adapter: AdapterCodex, SecretVersion: 1, Secret: codexLogin}
	f.SessionCredentials[item1] = []map[string]any{
		{"id": "cred-1", "label": "max-cloud", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": issuedSecret},
		{"id": plan.ID, "label": plan.Label, "vendor": "openai", "kind": "oauth_token", "serves": []any{"codex"}, "secret": codexLogin, "secretVersion": float64(1)},
	}
	limits, known := &Limits{}, &KnownAccounts{}
	if err := known.Note(IssuedAccount(plan), time.Now()); err != nil {
		t.Fatal(err)
	}
	loop := &Loop{Known: known, Limits: limits}
	ev := &Evaluator{GW: gw, Harness: claude, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, Model: "sonnet",
		Codex: ObserveLimits(second, limits), CodexModel: func(string) string { return "gpt-6-astra" }, CodexAccounts: loop.CodexCandidates, Limits: limits,
		Thresholds: func() LimitsConfig { return LimitsConfig{} }, Credentials: &Credentials{GW: gw, Limits: limits, Known: known, Log: log.New(io.Discard, "", 0)},
		Log: log.New(io.Discard, "", 0)}
	return f, ev, claude, limits
}

func evaluateItem1(t *testing.T, f *gatewaytest.Fake, ev *Evaluator) map[string]any {
	t.Helper()
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatal(err)
	}
	if n := nodeVerdicts(f); n != 0 {
		t.Fatalf("the node handed in %d verdicts of its own", n)
	}
	return evaluationClosed(t, f)
}

// failedByTheNode evaluates item 1 when no session can run — the branch
// cannot be diffed — and returns the one evaluateWorkItem the node sent:
// the item's own failure, which needs no evaluator to judge.
func failedByTheNode(t *testing.T, f *gatewaytest.Fake, ev *Evaluator) map[string]any {
	t.Helper()
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) != 1 {
		t.Fatalf("evaluate calls: %+v", calls)
	}
	return calls[0].Input
}

// evaluateJobsClosed counts the evaluate jobs the node closed.
func evaluateJobsClosed(f *gatewaytest.Fake) int {
	n := 0
	for _, c := range f.CallsNamed("finishJob") {
		if c.Input["kind"] == EvaluateKind {
			n++
		}
	}
	return n
}

// evaluationClosed is the one evaluate job the node closed: the session
// handed its verdict to the product's tool, and the node's finishJob
// carries only its notes tail (runsheet ADR 077). Its "notes" is that
// tail, and "finish" the whole finish.
func evaluationClosed(t *testing.T, f *gatewaytest.Fake) map[string]any {
	t.Helper()
	var closed []map[string]any
	for _, c := range f.CallsNamed("finishJob") {
		if c.Input["kind"] != EvaluateKind {
			continue
		}
		fin, _ := c.Input["finish"].(map[string]any)
		res, ok := fin["result"].(map[string]any)
		if !ok {
			t.Fatalf("an evaluate job closed with no result: %+v", fin)
		}
		closed = append(closed, map[string]any{"notes": res["notes_tail"], "costCents": res["cost_cents"], "finish": fin})
	}
	if len(closed) != 1 {
		t.Fatalf("evaluate jobs closed: %+v", closed)
	}
	return closed[0]
}

// replayCodex is the real Codex adapter on a `codex` that replays the
// recorded API-key stream (internal/harness/testdata), its final message
// replaced by text when text is not empty (JSON-escaped).
func replayCodex(t *testing.T, text string) harness.Harness {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "harness", "testdata", "codex_exec_apikey.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		data = []byte(strings.Replace(string(data), `"text":"hello from codex"`, `"text":"`+text+`"`, 1))
		// a scripted verdict goes to the node's mesh_result on the stream
		// (ADR-041), as the mcp_tool_call Codex records
		if v := scriptedVerdict(strings.ReplaceAll(text, `\n`, "\n")); v != nil {
			args, _ := json.Marshal(map[string]any{"verdict": v})
			call := `{"type":"item.completed","item":{"id":"i_result","type":"mcp_tool_call","server":"mesh-node","tool":"mesh_result","status":"completed","arguments":` + string(args) + `,"result":{"content":[{"type":"text","text":"recorded"}]}}}` + "\n"
			data = []byte(strings.Replace(string(data), `{"type":"turn.completed"`, call+`{"type":"turn.completed"`, 1))
		}
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(fixture, data, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat > /dev/null\ncat "+fixture+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_TEST_CODEX_KEY", "sk-test")
	return &harness.Codex{Binary: bin, HomeRoot: t.TempDir(), Account: harness.CodexAccount{Name: "plan"}}
}

// replayedCost is the recorded stream's cost at gpt-6-astra.
func replayedCost(t *testing.T) float64 {
	t.Helper()
	fh, err := os.Open(filepath.Join("..", "harness", "testdata", "codex_exec_apikey.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	return harness.ParseCodexStream(fh, "gpt-6-astra", nil).CostUSD
}

// diffWS is a workspace whose Diff answers err.
type diffWS struct {
	*fakeWS
	err error
}

func (w *diffWS) Diff(context.Context, string, string) (string, error) { return "", w.err }

func diffFailFixture(t *testing.T, err error) (*gatewaytest.Fake, *Evaluator, *fakeHarness) {
	t.Helper()
	f, ev, h, _ := secondFixture(t, "normal", "VERDICT: PASS", &fakeCodexH{})
	ev.WS = &diffWS{fakeWS: ev.WS.(*fakeWS), err: err}
	return f, ev, h
}

// A branch sharing no history with the base is the item's failure: one
// verdict, the reason in its notes, no session, and an Evaluate that
// returns rather than loops.
func TestEvaluateNoMergeBaseFailsTheItem(t *testing.T) {
	f, ev, h := diffFailFixture(t, &NoMergeBaseError{Base: "main", Branch: "mesh/ship-it-1"})
	in := failedByTheNode(t, f, ev)
	notes, _ := in["notes"].(string)
	if !strings.Contains(notes, "shares no history with origin/main") || !strings.Contains(notes, "rebuilt from scratch") || !strings.Contains(notes, "Rebuild it on origin/main") {
		t.Fatalf("verdict: %+v", in)
	}
	// the node's failed outcome names the clause the product fails it on:
	// a fail with no clause is refused (runsheet FailWithoutClausesRefused)
	if cl, _ := in["failedClauses"].([]any); len(cl) != 1 || cl[0].(map[string]any)["clause"] != "the branch can be evaluated: it diffs against its base" {
		t.Fatalf("failed clauses: %+v", in["failedClauses"])
	}
	// it goes as the job's close, not a verdict of the node's own
	if n := len(f.CallsNamed("finishJob")); n != 1 {
		t.Fatalf("finishJob calls = %d, want 1", n)
	}
	if len(h.jobs) != 0 {
		t.Fatalf("no evaluator session: %d jobs", len(h.jobs))
	}
	// a second call records one more verdict and returns; nothing loops inside
	f.WorkItems[item1]["status"] = "evaluating"
	item, _ := ev.GW.WorkItem(context.Background(), item1)
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatal(err)
	}
	if n := len(f.CallsNamed("evaluateWorkItem")); n != 2 {
		t.Fatalf("evaluate calls = %d, want 1 per Evaluate", n)
	}
}

func TestEvaluateOtherDiffErrorFailsTheItem(t *testing.T) {
	f, ev, h := diffFailFixture(t, errors.New("git diff: exit status 128: bad object"))
	in := failedByTheNode(t, f, ev)
	if in["notes"] != "the evaluation could not diff the branch against origin/main: git diff: exit status 128: bad object" {
		t.Fatalf("verdict: %+v", in)
	}
	if len(h.jobs) != 0 {
		t.Fatal("no evaluator session")
	}
}

// The console case: a branch forked from main, a shallow single-branch
// workspace. Prepare's checkout is as the node does it; the evaluator is
// handed the diff after the deepening.
func TestEvaluateConsoleCaseShallowSingleBranch(t *testing.T) {
	const branch = "mesh/merges-dont-wait-in-line-9"
	dir := mergeBaseFixtureBranch(t, false, branch)
	f, ev, h, _ := secondFixture(t, "normal", "ok\nVERDICT: PASS", &fakeCodexH{})
	ws := ev.WS.(*fakeWS)
	ev.WS = &gitDiffWS{fakeWS: ws, dir: dir, branch: branch}
	f.WorkItems[item1]["branch"] = branch
	evaluateItem1(t, f, ev)
	if len(h.jobs) != 1 || !strings.Contains(h.jobs[0].Prompt, "the branch's change") {
		t.Fatalf("the evaluator got no diff: %d jobs", len(h.jobs))
	}
}

// gitDiffWS computes Diff with real git in a prepared checkout.
type gitDiffWS struct {
	*fakeWS
	dir string
	// branch is the item's branch; the checkout must be on it.
	branch string
}

func (w *gitDiffWS) Diff(ctx context.Context, _, base string) (string, error) {
	if w.branch != "" {
		out, err := exec.Command("git", "-C", w.dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
		if err != nil || strings.TrimSpace(string(out)) != w.branch {
			return "", fmt.Errorf("checkout is on %q, the item's branch is %q", strings.TrimSpace(string(out)), w.branch)
		}
	}
	return (&Git{}).Diff(ctx, w.dir, base)
}

// alreadyDoneItem1 is item 1 finished already_done with evidence, as
// the builder hands it in (mesh ADR 003 decision 1): no branch.
func alreadyDoneItem1(t *testing.T, f *gatewaytest.Fake, ev *Evaluator) gateway.WorkItem {
	t.Helper()
	delete(f.WorkItems[item1], "branch")
	f.WorkItems[item1]["outcome"] = gateway.OutcomeAlreadyDone
	f.WorkItems[item1]["evidence"] = "https://github.com/acme/mesh/pull/41 (merged as 9f8e7d6) holds TestThing"
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	// the platform's reads carry outcome and evidence once schema.json
	// does (ADR 010); the node judges the row it is handed
	item.Outcome, item.Evidence = gateway.OutcomeAlreadyDone, "https://github.com/acme/mesh/pull/41 (merged as 9f8e7d6) holds TestThing"
	return *item
}

// An already_done item's evaluator is given the evidence and the
// acceptance to check on main, on main's checkout with no branch, and
// no diff; its pass is recorded with EvaluateWorkItem and nothing opens
// a pull request: the platform marks the item merged.
func TestEvaluateAlreadyDoneConfirmed(t *testing.T) {
	f, ev, h, _ := secondFixture(t, "normal", "PR 41 is on main and go test ./... passes there.\n\nVERDICT: PASS", &fakeCodexH{})
	ws := ev.WS.(*fakeWS)
	// the session hands its PASS to the product's tool (runsheet ADR 077)
	f.HandedInVerdicts = map[string]map[string]any{item1: {"passed": true, "notes": "PR 41 is on main and go test ./... passes there."}}
	if err := ev.Evaluate(context.Background(), alreadyDoneItem1(t, f, ev)); err != nil {
		t.Fatal(err)
	}
	if len(ws.prepared) != 1 || ws.prepared[0].Branch != "" || ws.prepared[0].BaseRef != "main" {
		t.Fatalf("prepared %+v: want main, no branch", ws.prepared)
	}
	for _, e := range ws.Events() {
		if e == "diff" {
			t.Fatal("an already_done item has no diff to compute")
		}
	}
	if len(h.jobs) != 1 {
		t.Fatalf("jobs: %d", len(h.jobs))
	}
	p := h.jobs[0].Prompt
	for _, want := range []string{
		"checking another session's claim that the item is already done",
		"# The claim",
		"https://github.com/acme/mesh/pull/41 (merged as 9f8e7d6) holds TestThing",
		"## Acceptance (this item)\n\nthe test fails",
		"`origin/main` checked out (detached) and its full history. There is no diff: check the claim on main.",
		"every cited pull request is merged into main",
		"every cited SHA is on it (`git merge-base --is-ancestor <sha> origin/main`)",
		"run the acceptance check yourself on main",
		"goes against an ADR", ".mesh/context/", "# How this job finishes", "mesh_verdict", "Change nothing",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	for _, not := range []string{"```diff", "+func TestThing", "origin/main...HEAD", "# The branch", "mesh/ship-it-1"} {
		if strings.Contains(p, not) {
			t.Fatalf("prompt has %q:\n%s", not, p)
		}
	}
	if in := evaluationClosed(t, f); in["notes"] != "(evaluated on Claude sonnet)" {
		t.Fatalf("closed %+v", in)
	}
	if n := len(f.CallsNamed("openPullRequest")); n != 0 || len(ws.pushed) != 0 {
		t.Fatalf("an already_done item opens no pull request: %d calls, pushed %v", n, ws.pushed)
	}
	if f.Item(item1)["status"] != "merged" {
		t.Fatalf("a confirmed already_done item is merged: %v", f.Item(item1)["status"])
	}
}

// A rejected claim goes back to ready through the ordinary failed
// evaluation, the notes saying what is missing on main.
func TestEvaluateAlreadyDoneRejected(t *testing.T) {
	f, ev, h, _ := secondFixture(t, "normal", "PR 41 is on main but TestThing is not: the acceptance does not hold there.\n\nVERDICT: FAIL", &fakeCodexH{})
	f.HandedInVerdicts = map[string]map[string]any{item1: {"passed": false, "notes": "PR 41 is on main but TestThing is not: the acceptance does not hold there."}}
	if err := ev.Evaluate(context.Background(), alreadyDoneItem1(t, f, ev)); err != nil {
		t.Fatal(err)
	}
	if in := evaluationClosed(t, f); in["notes"] != "(evaluated on Claude sonnet)" || in["costCents"] != float64(125) {
		t.Fatalf("closed %+v", in)
	}
	if f.Item(item1)["status"] != "ready" || len(h.jobs) != 1 {
		t.Fatalf("a rejected claim readies the item: %v", f.Item(item1)["status"])
	}
}

// The evaluation's usage record carries the goal's priority and the
// evaluator count (adr-016's amendment 2026-10-02): a normal goal with
// no second opinion logs evaluators=1, a high goal whose second opinion
// runs logs evaluators=2 — the measure the goal doc needs for the
// saving one evaluator for ordinary work makes.
func TestEvaluationUsageRecordsPriorityAndEvaluators(t *testing.T) {
	var buf bytes.Buffer
	f, ev, _, _ := secondFixture(t, "normal", "Holds.\n\nVERDICT: PASS", &fakeCodexH{text: "VERDICT: FAIL"})
	ev.Credentials.Log = log.New(&buf, "", log.LstdFlags)
	evaluateItem1(t, f, ev)
	if line := buf.String(); !strings.Contains(line, ": usage evaluate priority=normal evaluators=1 {") {
		t.Fatalf("logged record lacks priority=normal evaluators=1:\n%s", line)
	}
	recs, err := ReadUsage(&buf, UsageQuery{From: day("2000-01-01"), To: day("2100-01-01"), Kind: "evaluate"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs.Records) != 1 || recs.Records[0].Priority != "normal" || recs.Records[0].Evaluators != 1 {
		t.Fatalf("records: %+v", recs.Records)
	}

	buf.Reset()
	codex := &fakeCodexH{text: "Holds.\nVERDICT: PASS", cost: 0.40,
		rl: &harness.RateLimit{Status: "allowed", ObservedAt: time.Now()}}
	f, ev, _, _ = secondFixture(t, "high", "Holds.\n\nVERDICT: PASS", codex)
	ev.Credentials.Log = log.New(&buf, "", log.LstdFlags)
	evaluateItem1(t, f, ev)
	if line := buf.String(); !strings.Contains(line, ": usage evaluate priority=high evaluators=1 {") {
		t.Fatalf("logged record lacks priority=high evaluators=1:\n%s", line)
	}
	recs, err = ReadUsage(&buf, UsageQuery{From: day("2000-01-01"), To: day("2100-01-01"), Kind: "evaluate"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs.Records) != 1 || recs.Records[0].Priority != "high" || recs.Records[0].Evaluators != 1 {
		t.Fatalf("records: %+v", recs.Records)
	}
}

// sentNoVerdict says whether an evaluateWorkItem input is the no-verdict
// shape runsheet ADR 035 defines: passed false, noVerdict true, the
// reason as noVerdictReason and in the notes, and never `outcome`.
func sentNoVerdict(in map[string]any) bool {
	_, outcome := in["outcome"]
	return !outcome && in["passed"] == false && in[gateway.NoVerdictField] == true &&
		in[gateway.NoVerdictReasonField] != "" && in[gateway.NoVerdictReasonField] == in["notes"]
}

// An evaluation prepares and authorizes a checkout of its own, never
// the build's: its read token on the build's credential helper would
// refuse the build's push (goal a-build-pushes-before-it-finishes).
func TestEvaluationPreparesItsOwnCheckout(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node"}); err != nil {
		t.Fatal(err)
	}
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"

	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "Holds.\n\nVERDICT: PASS"}
	ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MCP: MeshContextHTTP(f.URL(), "rs_test"), MaxBudgetUSD: 2, Log: log.New(os.Stderr, "", 0)}
	item, err := gw.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatal(err)
	}

	build := WorkspaceSpec{Root: ws.root, GoalSlug: "ship-it", Seq: 1, Repo: "acme/widgets", Branch: "mesh/ship-it-1", BaseRef: "main"}.Dir()
	eval := EvalWorkspaceSpec(ws.root, "ship-it", 1, "acme/widgets", "mesh/ship-it-1", "main").Dir()
	if eval == build {
		t.Fatalf("the evaluation's dir is the build's: %s", eval)
	}
	if len(ws.prepared) != 1 || ws.prepared[0].Dir() != eval {
		t.Fatalf("prepared %+v, want %s", ws.prepared, eval)
	}
	if len(ws.authorized) == 0 {
		t.Fatal("the evaluation's checkout was never authorized")
	}
	for _, a := range ws.authorized {
		if !strings.HasSuffix(a, " "+eval) || strings.HasSuffix(a, " "+build) {
			t.Fatalf("authorized %q, want the evaluation's %s, never the build's %s", a, eval, build)
		}
	}
	if len(h.jobs) != 1 || h.jobs[0].Workspace != eval {
		t.Fatalf("job workspace %+v, want %s", h.jobs, eval)
	}
}
