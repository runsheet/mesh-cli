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
	loop.Tick(ctx)
	waitFor(t, func() bool { return f.Item(item1)["status"] == "review" })

	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) != 1 || calls[0].Input["aggregateId"] != item1 || calls[0].Input["passed"] != true || calls[0].Input["notes"] != "The test exists and fails for the right reason.\n\nevaluators: one — normal/low goal, no protected path\n(evaluated on the CLI's default model)" || calls[0].Input["costCents"] != float64(125) {
		t.Fatalf("evaluate calls: %+v", calls)
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
	for _, want := range []string{"Write the failing test", "the test fails", "go test ./... passes", "+func TestThing", "origin/main...HEAD", "# How this job finishes", "{verdict}", "Change nothing",
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

	// a failing verdict readies it again with the notes; no verdict leaves it
	f.WorkItems[item1]["status"] = "evaluating"
	h.text = "The test passes already, so it proves nothing.\nVERDICT: FAIL"
	loop.Tick(ctx)
	waitFor(t, func() bool { return f.Item(item1)["status"] == "ready" })
	if f.Item(item1)["evaluation"] != "The test passes already, so it proves nothing.\n\nevaluators: one — normal/low goal, no protected path\n(evaluated on the CLI's default model)" {
		t.Fatalf("notes: %v", f.Item(item1)["evaluation"])
	}
	// no VERDICT line is no verdict, with its reason and its cost: never
	// a fail, and the item stays in evaluating (ADR 011's amendment
	// 2026-09-29)
	f.WorkItems[item1]["status"] = "evaluating"
	h.text = "I could not tell."
	loop.Tick(ctx)
	loop.wg.Wait()
	calls = f.CallsNamed("evaluateWorkItem")
	if f.Item(item1)["status"] != "evaluating" || len(calls) != 3 {
		t.Fatalf("no verdict must leave the item in evaluating: %v, %d calls", f.Item(item1)["status"], len(calls))
	}
	in := calls[2].Input
	if !sentNoVerdict(in) || in["notes"] != "no result: the session made no mesh_result call" || in["costCents"] != float64(125) {
		t.Fatalf("no-verdict call: %+v", in)
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
	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) != 1 {
		t.Fatalf("evaluate calls: %+v", calls)
	}
	return calls[0].Input
}

// A high goal whose evaluation passes is evaluated again on Codex, with
// the same prompt in the same workspace; both pass, so it passes, with
// both verdicts labelled and the cost of both.
func TestSecondOpinionBothPass(t *testing.T) {
	codex := &fakeCodexH{text: "The test fails for the right reason.\nVERDICT: PASS", cost: 0.40,
		rl: &harness.RateLimit{Status: "allowed", ObservedAt: time.Now()}}
	f, ev, claude, limits := secondFixture(t, "high", "Holds.\n\nVERDICT: PASS", codex)
	in := evaluateItem1(t, f, ev)
	want := "Claude (first evaluation, sonnet): VERDICT: PASS\nHolds.\n\nCodex (second opinion, gpt-6-astra): VERDICT: PASS\nThe test fails for the right reason.\n\nevaluators: two — urgent/high goal\n(evaluated on sonnet; second opinion on Codex gpt-6-astra)"
	if in["passed"] != true || in["notes"] != want || in["costCents"] != float64(165) {
		t.Fatalf("recorded %+v\nwant notes %q", in, want)
	}
	if len(codex.jobs) != 1 || len(claude.jobs) != 1 {
		t.Fatalf("runs: claude %d, codex %d", len(claude.jobs), len(codex.jobs))
	}
	cj, xj := claude.jobs[0], codex.jobs[0]
	if xj.Prompt != cj.Prompt || xj.Workspace != cj.Workspace || xj.Kind != harness.KindEvaluate || xj.Model != "gpt-6-astra" {
		t.Fatalf("codex job %+v", xj)
	}
	// its reading is kept under its own key, apart from Claude's
	if _, ok := limits.Reading("codex:plan"); !ok {
		t.Fatal("the Codex reading is not kept under codex:plan")
	}
	if len(limits.Readings()) != 0 {
		t.Fatalf("a Codex reading is among Claude's: %+v", limits.Readings())
	}
}

// An urgent goal: Claude passes, Codex fails — a disagreement fails the
// evaluation, with both notes for the builder. Codex is replayed from
// the recorded stream, its final message a failing verdict.
func TestSecondOpinionDisagreementFails(t *testing.T) {
	codex := replayCodex(t, "Nothing asserts the error path.\\nVERDICT: FAIL")
	f, ev, _, _ := secondFixture(t, "urgent", "Holds.\n\nVERDICT: PASS", codex)
	in := evaluateItem1(t, f, ev)
	notes, _ := in["notes"].(string)
	if in["passed"] != false || !strings.Contains(notes, "Claude (first evaluation, sonnet): VERDICT: PASS\nHolds.") ||
		!strings.Contains(notes, "Codex (second opinion, gpt-6-astra): VERDICT: FAIL\nNothing asserts the error path.") {
		t.Fatalf("recorded %+v", in)
	}
	// the cost is Claude's and Codex's, at gpt-6-astra's list price
	want := Cents(1.25 + replayedCost(t))
	if in["costCents"] != float64(want) || want <= 125 {
		t.Fatalf("cost %v, want %d", in["costCents"], want)
	}
	if f.Item(item1)["status"] != "ready" {
		t.Fatalf("a failed evaluation readies the item: %v", f.Item(item1)["status"])
	}
}

// A normal or low goal's pass touching no protected path never runs
// Codex.
func TestSecondOpinionNotRun(t *testing.T) {
	for _, c := range []struct{ priority, first string }{
		{"normal", "Holds.\nVERDICT: PASS"},
		{"", "Holds.\nVERDICT: PASS"},
		{"low", "Holds.\nVERDICT: PASS"},
	} {
		codex := &fakeCodexH{text: "VERDICT: FAIL"}
		f, ev, _, _ := secondFixture(t, c.priority, c.first, codex)
		in := evaluateItem1(t, f, ev)
		if codex.runs() != 0 {
			t.Errorf("%q %q: Codex ran", c.priority, c.first)
		}
		notes, _ := in["notes"].(string)
		if strings.Contains(notes, "Codex") || in["costCents"] != float64(125) || in["passed"] != strings.HasSuffix(c.first, "PASS") {
			t.Errorf("%q: recorded %+v", c.priority, in)
		}
		if !strings.Contains(notes, "\n\nevaluators: one — normal/low goal, no protected path\n(evaluated on ") {
			t.Errorf("%q: notes lack the evaluators line:\n%s", c.priority, notes)
		}
	}
}

// A second opinion that errors or reaches no verdict keeps the first
// verdict and says so. The no-verdict one is the recorded stream as it
// is ("hello from codex").
func TestSecondOpinionUnavailable(t *testing.T) {
	for name, c := range map[string]struct {
		h    harness.Harness
		want string
	}{
		"error":      {&fakeCodexH{err: errors.New("harness codex: stream disconnected"), cost: 0.1}, "Codex second opinion unavailable: harness codex: stream disconnected"},
		"no verdict": {replayCodex(t, ""), "Codex second opinion unavailable: no result: the session made no mesh_result call"},
	} {
		f, ev, _, _ := secondFixture(t, "high", "Holds.\n\nVERDICT: PASS", c.h)
		in := evaluateItem1(t, f, ev)
		want := "Holds.\n\n" + c.want + "\n\nevaluators: two — urgent/high goal\n(evaluated on sonnet)"
		if in["passed"] != true || in["notes"] != want {
			t.Errorf("%s: recorded %+v\nwant notes %q", name, in, want)
		}
	}
}

// A Codex account that is stopped skips the second opinion, which the
// notes say — and never paces or stops a Claude session: the node's own
// pace and its choice of account read Claude's readings only.
func TestSecondOpinionAccountStopped(t *testing.T) {
	codex := &fakeCodexH{text: "VERDICT: FAIL"}
	f, ev, _, limits := secondFixture(t, "urgent", "Holds.\n\nVERDICT: PASS", codex)
	now := time.Now()
	limits.Observe("codex:plan", &harness.RateLimit{Status: "rejected", ResetsAt: now.Add(time.Hour), ObservedAt: now,
		FiveHour: harness.RateLimitWindow{Utilization: 1, ResetsAt: now.Add(time.Hour)}})
	in := evaluateItem1(t, f, ev)
	notes, _ := in["notes"].(string)
	if codex.runs() != 0 || in["passed"] != true || !strings.Contains(notes, "Codex second opinion unavailable: no Codex account has headroom") {
		t.Fatalf("codex runs %d, recorded %+v", codex.runs(), in)
	}
	// the loop — no Claude accounts configured, and so judged by its
	// strictest reading — is not paced by Codex's
	loop := &Loop{Cfg: &Config{}, Limits: limits}
	if p := loop.pacing(); p.mode != paceNormal {
		t.Fatalf("Claude paced by the Codex account: %+v", p)
	}
	if _, ok := loop.accountFor(context.Background(), "normal", false, ""); !ok {
		t.Fatal("no Claude account for normal work while only Codex is stopped")
	}
	// and on the heartbeat all the same
	if hb := limits.Heartbeat(nil, time.Now(), LimitsConfig{}); len(hb) != 1 || hb[0].Account != "codex:plan" {
		t.Fatalf("heartbeat %+v", hb)
	}
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

// Two long notes are truncated together: neither is cut out, and a
// short one leaves its room to the other.
func TestBothNotesTruncatedTogether(t *testing.T) {
	long := strings.Repeat("a", 5000)
	claude, codex := family{model: "sonnet"}, family{codex: true, model: "gpt-6-astra"}
	got := bothNotes(claude, long, true, codex, strings.Repeat("b", 5000), false, 3800)
	if len(got) > 3800 || strings.Count(got, "a") < 1800 || strings.Count(got, "b") < 1800 || !strings.Contains(got, "Codex (second opinion, gpt-6-astra): VERDICT: FAIL\nbbb") {
		t.Fatalf("len %d, a %d, b %d", len(got), strings.Count(got, "a"), strings.Count(got, "b"))
	}
	got = bothNotes(claude, long, true, codex, "short", true, 3800)
	if len(got) > 3800 || strings.Count(got, "a") < 3600 || !strings.HasSuffix(got, "VERDICT: PASS\nshort") {
		t.Fatalf("len %d: %q", len(got), got[len(got)-60:])
	}
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
	in := evaluateItem1(t, f, ev)
	notes, _ := in["notes"].(string)
	if in["passed"] != false || in["costCents"] != float64(0) || !strings.Contains(notes, "shares no history with origin/main") || !strings.Contains(notes, "rebuilt from scratch") || !strings.Contains(notes, "Rebuild it on origin/main") {
		t.Fatalf("verdict: %+v", in)
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
	in := evaluateItem1(t, f, ev)
	if in["passed"] != false || in["notes"] != "the evaluation could not diff the branch against origin/main: git diff: exit status 128: bad object" {
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
		"goes against an ADR", ".mesh/context/", "# How this job finishes", "{verdict}", "Change nothing",
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
	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) != 1 || calls[0].Input["passed"] != true || calls[0].Input["notes"] != "PR 41 is on main and go test ./... passes there.\n\nevaluators: one — normal/low goal, no protected path\n(evaluated on sonnet)" {
		t.Fatalf("evaluate calls: %+v", calls)
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
	if err := ev.Evaluate(context.Background(), alreadyDoneItem1(t, f, ev)); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("evaluateWorkItem")
	want := "PR 41 is on main but TestThing is not: the acceptance does not hold there.\n\nevaluators: one — normal/low goal, no protected path\n(evaluated on sonnet)"
	if len(calls) != 1 || calls[0].Input["aggregateId"] != item1 || calls[0].Input["passed"] != false || calls[0].Input["notes"] != want || calls[0].Input["costCents"] != float64(125) {
		t.Fatalf("evaluate calls: %+v", calls)
	}
	if f.Item(item1)["status"] != "ready" || len(h.jobs) != 1 {
		t.Fatalf("a rejected claim readies the item: %v", f.Item(item1)["status"])
	}
}

// An evaluation whose Codex second opinion also ran hands in one report
// for both: Codex's tokens and cost added to Claude's, as the cost is.
func TestSecondOpinionUsageInTheOneReport(t *testing.T) {
	codex := &fakeCodexH{text: "Holds.\nVERDICT: PASS", cost: 0.40,
		usage: &harness.Usage{Model: "gpt-6-astra", InputTokens: 1000, CachedInputTokens: 200, OutputTokens: 50}}
	f, ev, _, _ := secondFixture(t, "high", "Holds.\n\nVERDICT: PASS", codex)
	issueClaude(f, item1)
	in := evaluateItem1(t, f, ev)
	if in["costCents"] != float64(165) {
		t.Fatalf("recorded %+v", in)
	}
	if len(f.SessionUsage) != 1 || f.SessionUsage[0].Name != "evaluateWorkItem" {
		t.Fatalf("usage: %+v", f.SessionUsage)
	}
	u := f.SessionUsage[0].Input
	if u["listCostCents"] != float64(165) || u["inputTokens"] != float64(1000) || u["cacheReadTokens"] != float64(200) ||
		u["outputTokens"] != float64(50) || u["adapter"] != "claude-code" || u["credentialId"] != "cred-1" || u["credentialKind"] != "subscription" || u["actualCostCents"] != float64(0) {
		t.Fatalf("usage: %v", u)
	}
	checkNoOldUsage(t, f)
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
	if line := buf.String(); !strings.Contains(line, ": usage evaluate priority=high evaluators=2 {") {
		t.Fatalf("logged record lacks priority=high evaluators=2:\n%s", line)
	}
	recs, err = ReadUsage(&buf, UsageQuery{From: day("2000-01-01"), To: day("2100-01-01"), Kind: "evaluate"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs.Records) != 1 || recs.Records[0].Priority != "high" || recs.Records[0].Evaluators != 2 {
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

// The rule, with the reason each case is recorded under (goal
// one-evaluator-for-ordinary-work).
func TestSecondOpinionWanted(t *testing.T) {
	correctness := []harness.FailedClause{{Clause: "the evaluator has a test", Satisfy: "add TestSecondOpinionWanted"}}
	host := []harness.FailedClause{{Clause: "the mini's load p90 under 1.5 × cores", Satisfy: "a quieter mini"}}
	for _, c := range []struct {
		name string
		d    evalDecision
		want bool
		why  string
	}{
		{"urgent", evalDecision{Priority: "urgent", FirstPassed: true}, true, "urgent/high goal"},
		{"high", evalDecision{Priority: "high", FirstPassed: true}, true, "urgent/high goal"},
		{"high, first fail", evalDecision{Priority: "high", FailedClauses: host}, true, "urgent/high goal"},
		{"normal pass", evalDecision{Priority: "normal", FirstPassed: true, Changed: []string{"internal/node/evaluate.go"}}, false, oneEvaluatorReason},
		{"low docs pass", evalDecision{Priority: "low", FirstPassed: true, Changed: []string{"docs/handbook.md"}}, false, oneEvaluatorReason},
		{"no priority", evalDecision{FirstPassed: true}, false, oneEvaluatorReason},
		{"protectedFiles", evalDecision{Priority: "normal", FirstPassed: true, ProtectedFiles: []string{"schema.sql"}}, true, "protected path schema.sql"},
		{"protectedRulePaths", evalDecision{Priority: "normal", FirstPassed: true, ProtectedRulePaths: []string{"db/**/*.sql"}, Changed: []string{"README.md", "db/migrations/0042_items.sql"}}, true, "protected path db/migrations/0042_items.sql"},
		{"protectedPaths", evalDecision{Priority: "low", FirstPassed: true, ProtectedPaths: []string{".github/workflows/*"}, Changed: []string{".github/workflows/release.yml"}}, true, "protected path .github/workflows/release.yml"},
		{"protectedPaths, no match", evalDecision{Priority: "low", FirstPassed: true, ProtectedPaths: []string{".github/workflows/*"}, Changed: []string{"docs/handbook.md"}}, false, oneEvaluatorReason},
		{"first fail, correctness", evalDecision{Priority: "normal", FailedClauses: correctness}, true, "first verdict FAIL on a correctness clause"},
		{"first fail, correctness beside a reading", evalDecision{Priority: "normal", FailedClauses: append(append([]harness.FailedClause(nil), host...), correctness...)}, true, "first verdict FAIL on a correctness clause"},
		{"first fail, host reading only", evalDecision{Priority: "normal", FailedClauses: host}, false, oneEvaluatorReason},
		{"always", evalDecision{Priority: "low", FirstPassed: true, Setting: secondOpinionAlways}, true, "second_opinion: always"},
		{"rule", evalDecision{Priority: "low", FirstPassed: true, Setting: "rule"}, false, oneEvaluatorReason},
	} {
		got, why := secondOpinionWanted(c.d)
		if got != c.want || why != c.why {
			t.Errorf("%s: (%v, %q), want (%v, %q)", c.name, got, why, c.want, c.why)
		}
	}
}

// A low goal's docs-only item is evaluated once, and the notes and
// node.log say why.
func TestOneEvaluatorForALowDocsItem(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "low", "sonnet", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	ev.WS.(*fakeWS).changed = []string{"docs/handbook.md", "docs/adr/adr-099.md"}
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 0 {
		t.Fatalf("runs: codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	in := assertChoice(t, f, "gpt-6-luna", "evaluators: one — normal/low goal, no protected path")
	if in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}
	if !strings.Contains(logs.String(), "item ship-it/1: evaluators: one — normal/low goal, no protected path") {
		t.Errorf("node.log:\n%s", logs.String())
	}
}

// A high goal's platform item is evaluated twice.
func TestTwoEvaluatorsForAHighPlatformItem(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "high", "opus", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	ev.WS.(*fakeWS).changed = []string{"internal/gateway/client.go"}
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("runs: codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	in := assertChoice(t, f, "gpt-6-sol", "evaluators: two — urgent/high goal")
	if in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}
	if !strings.Contains(logs.String(), "item ship-it/1: evaluators: two — urgent/high goal") {
		t.Errorf("node.log:\n%s", logs.String())
	}
}

// A normal item whose first verdict fails a correctness clause is
// evaluated twice: both failing records the union of their clauses, and
// a second pass passes it with both notes.
func TestTwoEvaluatorsWhenTheFirstVerdictFails(t *testing.T) {
	second := "Vet fails too.\nFAILED: the evaluator has a test || SATISFY: add it\nFAILED: go vet passes || SATISFY: fix the vet finding\nVERDICT: FAIL"
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", second, correctnessFail)
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("runs: codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	in := assertChoice(t, f, "gpt-6-luna", "evaluators: two — first verdict FAIL on a correctness clause")
	got, _ := in[gateway.FailedClausesField].([]any)
	if in["passed"] != false || len(got) != 2 {
		t.Fatalf("recorded %+v", in)
	}
	for i, want := range []string{"the evaluator has a test", "go vet passes"} {
		if c, _ := got[i].(map[string]any); c["clause"] != want {
			t.Errorf("clause %d = %v, want %q", i, c, want)
		}
	}
	notes, _ := in["notes"].(string)
	if !strings.Contains(notes, "Codex (first evaluation, gpt-6-luna): VERDICT: FAIL\nNo test covers it.") || !strings.Contains(notes, "Claude (second opinion, sonnet): VERDICT: FAIL") {
		t.Fatalf("notes:\n%s", notes)
	}

	// a second pass passes it, both notes kept
	f, ev, _, _, _, _ = familyFixture(t, "normal", "sonnet", "The test is there.\nVERDICT: PASS", correctnessFail)
	evaluateItem1(t, f, ev)
	in = assertChoice(t, f, "gpt-6-luna", "evaluators: two — first verdict FAIL on a correctness clause")
	notes, _ = in["notes"].(string)
	if in["passed"] != true || in[gateway.FailedClausesField] != nil || !strings.Contains(notes, "VERDICT: FAIL\nNo test covers it.") || !strings.Contains(notes, "Claude (second opinion, sonnet): VERDICT: PASS\nThe test is there.") {
		t.Fatalf("recorded %+v", in)
	}
}

// A normal item whose first fail names only a reading of the host is
// evaluated once (ADR-020: never failed for the host's number, and not
// worth a second judge).
func TestOneEvaluatorOnAHostReadingFail(t *testing.T) {
	first := "The mini was busy.\nFAILED: the mini's load p90 under 1.5 × cores || SATISFY: a quieter mini\nVERDICT: FAIL"
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "VERDICT: PASS", first)
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 0 {
		t.Fatalf("runs: codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	if in := assertChoice(t, f, "gpt-6-luna", "evaluators: one — normal/low goal, no protected path"); in["passed"] != false {
		t.Fatalf("recorded %+v", in)
	}
}

// A normal item with protectedFiles is evaluated twice, as is one whose
// changed file matches the project's protectedPaths.
func TestTwoEvaluatorsOnAProtectedPath(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	f.WorkItems[item1]["protectedFiles"] = []any{"schema.sql"}
	f.WorkItems[item1]["protectedRulePaths"] = []any{"*.sql"}
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("protectedFiles: runs codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	if in := assertChoice(t, f, "gpt-6-luna", "evaluators: two — protected path schema.sql"); in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}

	f, ev, claude, codex, _, _ = familyFixture(t, "low", "sonnet", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	f.Projects[projID]["protectedPaths"] = []any{"db/migrations/**"}
	ev.WS.(*fakeWS).changed = []string{"docs/x.md", "db/migrations/0042.sql"}
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("protectedPaths: runs codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	if in := assertChoice(t, f, "gpt-6-luna", "evaluators: two — protected path db/migrations/0042.sql"); in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}

	// the checkout's own .mesh/config.yml protected_paths
	f, ev, claude, codex, _, _ = familyFixture(t, "normal", "sonnet", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	ws := ev.WS.(*fakeWS)
	writeMeshConfig(t, EvalWorkspaceSpec(ws.root, "ship-it", 1, "", "", "").Dir(), "protected_paths:\n  - schema.json\n  - internal/gateway/fields/\n")
	ws.changed = []string{"README.md", "internal/gateway/fields/WorkItemIndex.txt"}
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("protected_paths: runs codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	if in := assertChoice(t, f, "gpt-6-luna", "evaluators: two — protected path internal/gateway/fields/WorkItemIndex.txt"); in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}

	// and a checkout whose protected_paths the diff misses gets one
	f, ev, claude, codex, _, _ = familyFixture(t, "normal", "sonnet", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	ws = ev.WS.(*fakeWS)
	writeMeshConfig(t, EvalWorkspaceSpec(ws.root, "ship-it", 1, "", "", "").Dir(), "protected_paths: [schema.json]\n")
	ws.changed = []string{"docs/handbook.md"}
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 0 {
		t.Fatalf("protected_paths, no match: runs codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	if in := assertChoice(t, f, "gpt-6-luna", "evaluators: one — normal/low goal, no protected path"); in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}
}

// A normal item under a checkout whose .mesh/config.yml says
// second_opinion: always is evaluated twice.
func TestSecondOpinionAlways(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	ws := ev.WS.(*fakeWS)
	dir := EvalWorkspaceSpec(ws.root, "ship-it", 1, "", "", "").Dir()
	writeMeshConfig(t, dir, "second_opinion: always\n")
	evaluateItem1(t, f, ev)
	if len(ws.prepared) != 1 || ws.prepared[0].Dir() != dir {
		t.Fatalf("prepared %+v, want %s", ws.prepared, dir)
	}
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("runs: codex %d, claude %d", codex.runs(), len(claude.jobs))
	}
	if in := assertChoice(t, f, "gpt-6-luna", "evaluators: two — second_opinion: always"); in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}
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
