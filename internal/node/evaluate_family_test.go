package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/crash"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/version"
)

// familyFixture is an evaluator on item 1 of a goal of that priority on
// a node that knows one issued Codex account, chatgpt-pro (oa-1), beside
// the Claude one the platform issues: Claude says claudeSays, Codex
// codexSays, the node's evaluate model is model, and the Codex
// readings land in the returned Limits under codex:chatgpt-pro.
func familyFixture(t *testing.T, priority, model, claudeSays, codexSays string) (*gatewaytest.Fake, *Evaluator, *fakeHarness, *fakeCodexH, *Limits, *syncBuf) {
	t.Helper()
	cacheRoot(t)
	f := seed(t)
	f.Goals[goalID]["priority"] = priority
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	f.SessionCredentials[item1] = []map[string]any{
		{"id": "cred-1", "label": "max-cloud", "vendor": "anthropic", "kind": "subscription", "serves": []any{"claude-code"}, "secret": issuedSecret},
		{"id": "oa-1", "label": "chatgpt-pro", "vendor": "openai", "kind": "oauth_token", "serves": []any{"codex"}, "secret": codexLogin, "secretVersion": float64(1)},
	}
	gw := gateway.New(f.URL(), "rs_test")
	limits, known := &Limits{}, &KnownAccounts{}
	if err := known.Note(IssuedAccount(codexSub()), time.Now()); err != nil {
		t.Fatal(err)
	}
	loop := &Loop{Known: known, Limits: limits}
	logs := &syncBuf{}
	ws := &fakeWS{root: t.TempDir()}
	claude := &fakeHarness{text: claudeSays}
	codex := &fakeCodexH{text: codexSays, cost: 0.40, usage: &harness.Usage{InputTokens: 1000, OutputTokens: 50}}
	ev := &Evaluator{GW: gw, Harness: claude, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, Model: model,
		Codex: ObserveLimits(codex, limits), CodexModel: CodexConfig{}.CodexModel, CodexAccount: CodexLimitsAccount(""),
		CodexAccounts: loop.CodexCandidates, Waits: &OncePer{}, Limits: limits, Thresholds: func() LimitsConfig { return LimitsConfig{} },
		Credentials: &Credentials{GW: gw, Limits: limits, Known: known, Log: log.New(logs, "", 0)}, Log: log.New(logs, "", 0)}
	return f, ev, claude, codex, limits, logs
}

// usageOf is the one usage report evaluateWorkItem carried.
func usageOf(t *testing.T, f *gatewaytest.Fake) map[string]any {
	t.Helper()
	if len(f.SessionUsage) != 1 || f.SessionUsage[0].Name != "evaluateWorkItem" {
		t.Fatalf("usage: %+v", f.SessionUsage)
	}
	return f.SessionUsage[0].Input
}

// (1) A known Codex account with headroom takes a normal item's
// evaluation: the Codex harness runs on the issued login's CODEX_HOME,
// on the Codex tier of the evaluate model (sonnet → gpt-6-luna), and
// the report handed in names adapter codex, gpt-6-luna and the credential.
func TestEvaluationRunsOnCodexWithHeadroom(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", "Holds.\nVERDICT: PASS")
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 0 || codex.runs() != 1 {
		t.Fatalf("runs: claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	j := codex.jobs[0]
	if j.Model != "gpt-6-luna" || j.Outlive {
		t.Fatalf("codex job model %q outlive %v", j.Model, j.Outlive)
	}
	if _, ok := envOf(j.Env, "CODEX_HOME"); !ok {
		t.Error("the Codex evaluation has no CODEX_HOME of the issued login")
	}
	if in["passed"] != true || in["notes"] != "Holds.\n\n(evaluated on Codex gpt-6-luna)" || in["costCents"] != float64(40) {
		t.Fatalf("recorded %+v", in)
	}
	u := usageOf(t, f)
	if u["adapter"] != "codex" || u["model"] != "gpt-6-luna" || u["credentialId"] != "oa-1" || u["credentialKind"] != "subscription" || u["inputTokens"] != float64(1000) {
		t.Fatalf("usage: %v", u)
	}
	for _, want := range []string{"item ship-it/1: evaluate on Codex gpt-6-luna: mesh/ship-it-1", "item ship-it/1: evaluation passed on Codex gpt-6-luna"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("node.log lacks %q:\n%s", want, logs.String())
		}
	}
}

// (2) A Codex account past pace_five_hour takes only urgent and high
// work: a normal item evaluates on Claude and reports claude-code, an
// urgent one still takes Codex.
func TestPacedCodexFallsBackToClaudeForNormalWork(t *testing.T) {
	paced := func(limits *Limits) {
		now := time.Now()
		limits.Observe("codex:chatgpt-pro", &harness.RateLimit{Status: "allowed", ObservedAt: now,
			FiveHour: harness.RateLimitWindow{Utilization: 0.85, ResetsAt: now.Add(2 * time.Hour)}})
	}
	f, ev, claude, codex, limits, _ := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: FAIL")
	paced(limits)
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 1 || codex.runs() != 0 {
		t.Fatalf("normal: runs claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	if in["notes"] != "Holds.\n\n(evaluated on sonnet)" {
		t.Fatalf("normal: recorded %+v", in)
	}
	if u := usageOf(t, f); u["adapter"] != "claude-code" || u["credentialId"] != "cred-1" {
		t.Fatalf("normal: usage %v", u)
	}

	f, ev, claude, codex, limits, _ = familyFixture(t, "urgent", "sonnet", "Holds.\nVERDICT: PASS", "Holds too.\nVERDICT: PASS")
	paced(limits)
	evaluateItem1(t, f, ev)
	if codex.runs() != 1 || codex.jobs[0].Model != "gpt-6-luna" {
		t.Fatalf("urgent: codex runs %d", codex.runs())
	}
	if u := usageOf(t, f); u["adapter"] != "codex" || u["model"] != "gpt-6-luna" {
		t.Fatalf("urgent: usage %v", u)
	}
}

// (3) An item pinned to a Codex model whose Codex account is stopped is
// not evaluated: it stays in evaluating for a later tick, nothing runs,
// and the wait is said once however many ticks try it.
func TestCodexPinWaitsWhenStopped(t *testing.T) {
	f, ev, claude, codex, limits, logs := familyFixture(t, "high", "sonnet", "VERDICT: PASS", "VERDICT: PASS")
	f.WorkItems[item1]["evaluateModel"] = "gpt-6-astra"
	now := time.Now()
	limits.Observe("codex:chatgpt-pro", &harness.RateLimit{Status: "rejected", ResetsAt: now.Add(time.Hour), ObservedAt: now,
		FiveHour: harness.RateLimitWindow{Utilization: 1, ResetsAt: now.Add(time.Hour)}})
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := ev.Evaluate(context.Background(), *item); !errors.Is(err, ErrFamilyWaits) {
			t.Fatalf("Evaluate = %v, want ErrFamilyWaits", err)
		}
	}
	if len(claude.jobs) != 0 || codex.runs() != 0 || len(f.CallsNamed("evaluateWorkItem")) != 0 || f.Item(item1)["status"] != "evaluating" {
		t.Fatalf("runs claude %d codex %d, verdicts %d, status %v", len(claude.jobs), codex.runs(), len(f.CallsNamed("evaluateWorkItem")), f.Item(item1)["status"])
	}
	if n := strings.Count(logs.String(), "pinned to Codex gpt-6-astra, and no Codex account has headroom"); n != 1 {
		t.Fatalf("said %d times:\n%s", n, logs.String())
	}

	// with headroom again it evaluates on the pinned model
	limits.Observe("codex:chatgpt-pro", &harness.RateLimit{Status: "allowed", ObservedAt: time.Now().Add(time.Second)})
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatal(err)
	}
	if codex.runs() != 1 || codex.jobs[0].Model != "gpt-6-astra" {
		t.Fatalf("codex runs %d", codex.runs())
	}
}

// (4) An item pinned to a Claude model never runs on Codex, even with
// Codex headroom; a normal goal gets no second opinion.
func TestClaudePinNeverRunsOnCodex(t *testing.T) {
	for _, pin := range []map[string]any{{"evaluateModel": "claude-opus-5"}, {"model": "claude-opus-5"}} {
		f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: FAIL")
		for k, v := range pin {
			f.WorkItems[item1][k] = v
		}
		in := evaluateItem1(t, f, ev)
		if len(claude.jobs) != 1 || codex.runs() != 0 || claude.jobs[0].Model != "claude-opus-5" {
			t.Fatalf("%v: runs claude %d, codex %d", pin, len(claude.jobs), codex.runs())
		}
		if u := usageOf(t, f); u["adapter"] != "claude-code" || in["passed"] != true {
			t.Fatalf("%v: usage %v", pin, u)
		}
	}
}

// (5) An urgent item evaluated first on Codex gets its second opinion
// on Claude, on its own Claude account: the notes name both families
// and models, and the one report is Codex's with Claude's folded in.
func TestCodexFirstClaudeSecondOpinion(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "urgent", "opus", "Also holds.\nVERDICT: PASS", "Holds.\nVERDICT: PASS")
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("runs: claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	xj, cj := codex.jobs[0], claude.jobs[0]
	if xj.Model != "gpt-6-sol" || cj.Model != "opus" || xj.Prompt != cj.Prompt || xj.Workspace != cj.Workspace {
		t.Fatalf("codex %q, claude %q", xj.Model, cj.Model)
	}
	if _, ok := envOf(cj.Env, "CODEX_HOME"); ok {
		t.Error("the Claude second opinion carries the Codex home")
	}
	if tok, _ := envOf(cj.Env, "CLAUDE_CODE_OAUTH_TOKEN"); tok != issuedSecret {
		t.Error("the Claude second opinion is not on the issued Claude account")
	}
	want := "Codex (first evaluation, gpt-6-sol): VERDICT: PASS\nHolds.\n\nClaude (second opinion, opus): VERDICT: PASS\nAlso holds.\n\n(evaluated on Codex gpt-6-sol; second opinion on Claude opus)"
	if in["passed"] != true || in["notes"] != want || in["costCents"] != float64(165) {
		t.Fatalf("recorded %+v\nwant notes %q", in, want)
	}
	u := usageOf(t, f)
	if u["adapter"] != "codex" || u["model"] != "gpt-6-sol" || u["credentialId"] != "oa-1" || u["listCostCents"] != float64(165) {
		t.Fatalf("usage: %v", u)
	}

	// a Claude fail fails it; the unavailable line names Claude
	f, ev, claude, _, _, _ = familyFixture(t, "urgent", "opus", "Nothing asserts it.\nVERDICT: FAIL", "Holds.\nVERDICT: PASS")
	in = evaluateItem1(t, f, ev)
	if notes, _ := in["notes"].(string); in["passed"] != false || !strings.Contains(notes, "Claude (second opinion, opus): VERDICT: FAIL\nNothing asserts it.") {
		t.Fatalf("recorded %+v", in)
	}
	f, ev, claude, _, _, _ = familyFixture(t, "urgent", "opus", "", "Holds.\nVERDICT: PASS")
	claude.text = "no verdict here"
	in = evaluateItem1(t, f, ev)
	if notes, _ := in["notes"].(string); in["passed"] != true || !strings.Contains(notes, "Claude second opinion unavailable: no verdict: no verdict here") {
		t.Fatalf("recorded %+v", in)
	}
}

// A node with no Codex account known and no codex: block evaluates on
// Claude as it always has, even with the Codex harness built: no Codex
// run and no second-opinion line.
func TestNoCodexAccountEvaluatesAsBefore(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "urgent", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: FAIL")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 1 || codex.runs() != 0 || in["notes"] != "Holds.\n\n(evaluated on sonnet)" {
		t.Fatalf("runs claude %d codex %d, recorded %+v", len(claude.jobs), codex.runs(), in)
	}
}

// The loop starts an evaluation no Claude account has room for when a
// Codex account has room for it (Loop.CodexRoom), and counts it as
// waiting otherwise.
func TestEvaluationStartsOnCodexRoom(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(0),
		Accounts: []AccountConfig{{Label: "max", Kind: AccountSubscription, TokenFile: "/dev/null"}}}
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	limits := &Limits{}
	now := time.Now()
	limits.Observe("max", &harness.RateLimit{Status: "rejected", ResetsAt: now.Add(time.Hour), ObservedAt: now,
		FiveHour: harness.RateLimitWindow{Utilization: 1, ResetsAt: now.Add(time.Hour)}})
	started := make(chan string, 1)
	room := false
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Limits: limits, Log: log.New(&syncBuf{}, "", 0),
		Evaluate:  func(_ context.Context, item gateway.WorkItem) { started <- item.ID },
		CodexRoom: func(string) bool { return room }}
	loop.waiting = map[string]int{}
	loop.evaluateHeld(context.Background())
	loop.wg.Wait()
	if len(started) != 0 || loop.waiting["evaluation"] != 1 {
		t.Fatalf("started %d, waiting %v", len(started), loop.waiting)
	}
	room = true
	loop.evaluateHeld(context.Background())
	loop.wg.Wait()
	if len(started) != 1 {
		t.Fatal("no evaluation started on Codex room")
	}
}

// fakePreflight is a Codex preflight that answers err and records the
// jobs it checked.
type fakePreflight struct {
	mu   sync.Mutex
	err  error
	jobs []harness.Job
}

func (p *fakePreflight) check(_ context.Context, job harness.Job) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jobs = append(p.jobs, job)
	return p.err
}

// A Codex account with headroom but a preflight that fails: the
// unpinned item is evaluated on Claude, the verdict says so and why, and
// node.log names the reason; nothing ran on Codex.
func TestCodexPreflightFailureEvaluatesOnClaude(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: FAIL")
	pf := &fakePreflight{err: errors.New("refuses auth: 401 Unauthorized: bad token")}
	ev.CodexPreflight = pf.check
	in := evaluateItem1(t, f, ev)
	if len(pf.jobs) != 1 || codex.runs() != 0 || len(claude.jobs) != 1 {
		t.Fatalf("preflights %d, runs claude %d, codex %d", len(pf.jobs), len(claude.jobs), codex.runs())
	}
	if _, ok := envOf(pf.jobs[0].Env, "CODEX_HOME"); !ok {
		t.Error("the preflight did not check the issued login's CODEX_HOME")
	}
	if _, ok := envOf(claude.jobs[0].Env, "CODEX_HOME"); ok {
		t.Error("the Claude session carries the Codex home")
	}
	if !claude.jobs[0].Outlive || claude.jobs[0].Model != "sonnet" {
		t.Fatalf("claude job model %q outlive %v", claude.jobs[0].Model, claude.jobs[0].Outlive)
	}
	want := "Holds.\n\n(evaluated on Claude sonnet; the Codex preflight failed: refuses auth: 401 Unauthorized: bad token)"
	if in["passed"] != true || in["notes"] != want {
		t.Fatalf("recorded %+v\nwant notes %q", in, want)
	}
	if u := usageOf(t, f); u["adapter"] != "claude-code" || u["credentialId"] != "cred-1" {
		t.Fatalf("usage %v", u)
	}
	if want := "item ship-it/1: Codex preflight failed (refuses auth: 401 Unauthorized: bad token); evaluating on Claude"; !strings.Contains(logs.String(), want) {
		t.Fatalf("node.log lacks %q:\n%s", want, logs.String())
	}

	// an urgent item gets no Codex second opinion after it: the same
	// preflight has just failed
	f, ev, claude, codex, _, _ = familyFixture(t, "urgent", "opus", "Holds.\nVERDICT: PASS", "VERDICT: FAIL")
	ev.CodexPreflight = (&fakePreflight{err: errors.New("unreachable: connection refused")}).check
	in = evaluateItem1(t, f, ev)
	if codex.runs() != 0 || len(claude.jobs) != 1 {
		t.Fatalf("urgent: runs claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	if notes, _ := in["notes"].(string); in["passed"] != true || !strings.Contains(notes, "Codex second opinion unavailable: preflight unreachable: connection refused") {
		t.Fatalf("urgent: recorded %+v", in)
	}
}

// A pinned gpt-… item whose preflight fails records no verdict and waits
// in evaluating, said once; a passing preflight then runs Codex as before.
func TestCodexPinWaitsOnFailedPreflight(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: PASS", "Holds.\nVERDICT: PASS")
	f.WorkItems[item1]["evaluateModel"] = "gpt-6-astra"
	pf := &fakePreflight{err: errors.New("unreachable: dial tcp: connection refused")}
	ev.CodexPreflight = pf.check
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := ev.Evaluate(context.Background(), *item); !errors.Is(err, ErrFamilyWaits) || !strings.Contains(err.Error(), "unreachable") {
			t.Fatalf("Evaluate = %v, want ErrFamilyWaits naming the preflight", err)
		}
	}
	if len(claude.jobs) != 0 || codex.runs() != 0 || len(f.CallsNamed("evaluateWorkItem")) != 0 || f.Item(item1)["status"] != "evaluating" {
		t.Fatalf("runs claude %d codex %d, verdicts %d, status %v", len(claude.jobs), codex.runs(), len(f.CallsNamed("evaluateWorkItem")), f.Item(item1)["status"])
	}
	if n := strings.Count(logs.String(), "pinned to Codex gpt-6-astra, and the Codex preflight failed (unreachable: dial tcp: connection refused)"); n != 1 {
		t.Fatalf("said %d times:\n%s", n, logs.String())
	}

	pf.err = nil
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatal(err)
	}
	if codex.runs() != 1 || codex.jobs[0].Model != "gpt-6-astra" || len(claude.jobs) != 0 {
		t.Fatalf("runs codex %d claude %d", codex.runs(), len(claude.jobs))
	}
	if in := f.CallsNamed("evaluateWorkItem")[0].Input; in["notes"] != "Holds.\n\n(evaluated on Codex gpt-6-astra)" {
		t.Fatalf("recorded %+v", in)
	}
}

// A passing preflight runs Codex as before, checking the very job Codex
// then runs; a Claude-first evaluation's Codex second opinion is
// preflighted too, and a failure makes it unavailable.
func TestCodexPreflightPassesAndGuardsSecondOpinion(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", "Holds.\nVERDICT: PASS")
	pf := &fakePreflight{}
	ev.CodexPreflight = pf.check
	in := evaluateItem1(t, f, ev)
	if len(pf.jobs) != 1 || codex.runs() != 1 || len(claude.jobs) != 0 || in["notes"] != "Holds.\n\n(evaluated on Codex gpt-6-luna)" {
		t.Fatalf("preflights %d, runs codex %d claude %d, recorded %+v", len(pf.jobs), codex.runs(), len(claude.jobs), in)
	}
	if h, _ := envOf(pf.jobs[0].Env, "CODEX_HOME"); h == "" || h != mustEnv(t, codex.jobs[0].Env, "CODEX_HOME") || pf.jobs[0].Model != codex.jobs[0].Model {
		t.Fatal("the preflight checked another job than the one Codex ran")
	}

	// Claude first (no Codex room for the first), Codex's second opinion
	// preflighted and refused
	f, ev, claude, codex, _, _ = familyFixture(t, "urgent", "opus", "Holds.\nVERDICT: PASS", "VERDICT: PASS")
	f.WorkItems[item1]["evaluateModel"] = "claude-opus-5"
	pf = &fakePreflight{err: errors.New("refuses auth: 403 Forbidden: no")}
	ev.CodexPreflight = pf.check
	in = evaluateItem1(t, f, ev)
	if len(pf.jobs) != 1 || codex.runs() != 0 || len(claude.jobs) != 1 {
		t.Fatalf("second: preflights %d, runs codex %d claude %d", len(pf.jobs), codex.runs(), len(claude.jobs))
	}
	if notes, _ := in["notes"].(string); in["passed"] != true || !strings.Contains(notes, "Codex second opinion unavailable: preflight refuses auth: 403 Forbidden: no") {
		t.Fatalf("second: recorded %+v", in)
	}
}

func mustEnv(t *testing.T, env []string, name string) string {
	t.Helper()
	v, ok := envOf(env, name)
	if !ok {
		t.Fatalf("no %s", name)
	}
	return v
}

// noContext is a built-in context server every one of whose calls
// errored.
func noContext(calls int, why string) map[string]*harness.ToolStat {
	errs := calls
	if calls == 0 {
		errs = 1
	}
	return map[string]*harness.ToolStat{"mesh-context": {Calls: calls, Errors: errs, Wall: 30 * time.Millisecond, FirstError: why}}
}

// toolRow is the usage report's row for tool.
func toolRow(t *testing.T, u map[string]any, tool string) map[string]any {
	t.Helper()
	rows, _ := u["tools"].([]any)
	for _, r := range rows {
		if row, _ := r.(map[string]any); row["tool"] == tool {
			return row
		}
	}
	t.Fatalf("usage has no %s row: %v", tool, u["tools"])
	return nil
}

// A Codex evaluation whose context server errored on every call judged
// blind: its FAIL is not recorded, the same job runs on Claude, and
// Claude's verdict is the verdict, the Codex attempt first in the notes
// and both sessions' tools in the one report.
func TestCodexWithoutContextIsNotAVerdict(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "Nothing to read.\nVERDICT: FAIL")
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	codex.tools = noContext(3, "Auth required during MCP startup")
	claude.tools = map[string]*harness.ToolStat{"mesh-context": {Calls: 2, Wall: 40 * time.Millisecond}}
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("runs: claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	xj, cj := codex.jobs[0], claude.jobs[0]
	if xj.Prompt != cj.Prompt || xj.Workspace != cj.Workspace || cj.Model != "sonnet" {
		t.Fatalf("claude job: model %q, same prompt %v", cj.Model, xj.Prompt == cj.Prompt)
	}
	if tok, _ := envOf(cj.Env, "CLAUDE_CODE_OAUTH_TOKEN"); tok != issuedSecret {
		t.Error("the Claude evaluation is not on the issued Claude account")
	}
	want := "Codex (first attempt, gpt-6-luna): no verdict — mesh-context: Auth required during MCP startup\n\nHolds.\n\n(evaluated on sonnet)\ntools: mesh-context: first call errored: Auth required during MCP startup"
	if in["passed"] != true || in["notes"] != want || in["costCents"] != float64(165) {
		t.Fatalf("recorded %+v\nwant notes %q", in, want)
	}
	if row := toolRow(t, usageOf(t, f), "mesh-context"); row["calls"] != 5.0 || row["errors"] != 3.0 {
		t.Fatalf("mesh-context row %v", row)
	}
	for _, s := range []string{
		"item ship-it/1: evaluation on Codex gpt-6-luna saw no context (mesh-context: Auth required during MCP startup); not a verdict, evaluating on Claude sonnet",
		"item ship-it/1: evaluation passed on Claude sonnet",
	} {
		if !strings.Contains(logs.String(), s) {
			t.Errorf("node.log lacks %q:\n%s", s, logs.String())
		}
	}
}

// The same with Claude first: its context server never connected, its
// FAIL is not recorded, and Codex (on the harness's own account) gives
// the verdict.
func TestClaudeWithoutContextRunsOnCodex(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", "Holds.\nVERDICT: PASS")
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	ev.CodexAccounts = nil // Codex only after Claude, on its own account
	claude.tools = noContext(0, "failed")
	codex.tools = map[string]*harness.ToolStat{"mesh-context": {Calls: 3}}
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 || codex.jobs[0].Prompt != claude.jobs[0].Prompt {
		t.Fatalf("runs: claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	want := "Claude (first attempt, sonnet): no verdict — mesh-context: failed\n\nHolds.\n\n(evaluated on Codex gpt-6-luna)\ntools: mesh-context served, never called: failed"
	if in["passed"] != true || in["notes"] != want {
		t.Fatalf("recorded %+v\nwant notes %q", in, want)
	}
	if row := toolRow(t, usageOf(t, f), "mesh-context"); row["calls"] != 3.0 || row["errors"] != 1.0 {
		t.Fatalf("mesh-context row %v", row)
	}
	if !strings.Contains(logs.String(), "evaluation on Claude sonnet saw no context (mesh-context: failed); not a verdict, evaluating on Codex gpt-6-luna") {
		t.Errorf("node.log:\n%s", logs.String())
	}
}

// With no other family to run on, an evaluation that saw no context
// is no verdict: recorded with the tool's error as its reason and its
// cost, never a fail, and the item stays in evaluating (ADR 011's
// amendment 2026-09-29).
func TestNoContextWithNoOtherFamilyIsNoVerdict(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", "VERDICT: PASS")
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	claude.tools = noContext(3, "Auth required")
	ev.Blind = &OncePer{}
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 1 || codex.runs() != 0 {
		t.Fatalf("runs claude %d codex %d", len(claude.jobs), codex.runs())
	}
	reason := "evaluation on Claude sonnet saw no context (mesh-context: Auth required); not a verdict, and Codex cannot evaluate it: the node has no Codex harness or account"
	if !sentNoVerdict(in) || in["notes"] != reason || in["costCents"] != float64(125) {
		t.Fatalf("recorded %+v\nwant notes %q", in, reason)
	}
	if f.Item(item1)["status"] != "evaluating" {
		t.Fatalf("status %v", f.Item(item1)["status"])
	}
	if row := toolRow(t, usageOf(t, f), "mesh-context"); row["errors"] != 3.0 {
		t.Fatalf("mesh-context row %v", row)
	}
	if !strings.Contains(logs.String(), "item ship-it/1: evaluation reached no verdict on Claude sonnet, recorded as no verdict ($1.25): "+reason) {
		t.Fatalf("node.log:\n%s", logs.String())
	}
}

// On a platform that takes no no-verdict outcome, an evaluation that saw
// no context and had no other family records nothing, as before: the
// item stays in evaluating, the wait is said once for it however many
// ticks try, and the platform's lack once per run.
func TestNoContextWithNoOtherFamilyWaitsOnAnOlderPlatform(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", "VERDICT: PASS")
	f.NoNoVerdict = true
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	claude.tools = noContext(3, "Auth required")
	ev.Blind, ev.NoVerdict = &OncePer{}, &OncePer{}
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ev.Evaluate(context.Background(), *item); !errors.Is(err, ErrNoContext) {
			t.Fatalf("Evaluate = %v, want ErrNoContext", err)
		}
	}
	if len(claude.jobs) != 2 || codex.runs() != 0 || len(f.CallsNamed("evaluateWorkItem")) != 0 || f.Item(item1)["status"] != "evaluating" {
		t.Fatalf("runs claude %d codex %d, verdicts %d, status %v", len(claude.jobs), codex.runs(), len(f.CallsNamed("evaluateWorkItem")), f.Item(item1)["status"])
	}
	for _, line := range []string{
		"item ship-it/1: evaluation on Claude sonnet saw no context (mesh-context: Auth required); not a verdict, and Codex cannot evaluate it: the node has no Codex harness or account; it stays in evaluating",
		"the platform takes no no-verdict outcome; a no-verdict evaluation waits in evaluating (said once per run)",
	} {
		if n := strings.Count(logs.String(), line); n != 1 {
			t.Fatalf("%q said %d times:\n%s", line, n, logs.String())
		}
	}
}

// A session whose stream carries no VERDICT line is no verdict: one
// evaluateWorkItem with passed false, noVerdict true and the reason
// `no VERDICT line: …` (runsheet ADR 035), never a bare fail; the item
// stays in evaluating and the platform keeps the reason.
func TestNoVerdictLineIsRecordedAsNoVerdict(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "I ran out of ideas.", "VERDICT: PASS")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 1 || codex.runs() != 0 {
		t.Fatalf("runs claude %d codex %d", len(claude.jobs), codex.runs())
	}
	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) != 1 {
		t.Fatalf("%d evaluateWorkItem calls", len(calls))
	}
	reason := "no VERDICT line: I ran out of ideas."
	if !sentNoVerdict(in) || in[gateway.NoVerdictReasonField] != reason || in["costCents"] != float64(125) {
		t.Fatalf("recorded %+v\nwant reason %q", in, reason)
	}
	item := f.Item(item1)
	if item["status"] != "evaluating" || item["noVerdictReason"] != reason || fmt.Sprint(item["noVerdicts"]) != "1" {
		t.Fatalf("item %+v", item)
	}
	if !strings.Contains(logs.String(), "evaluation reached no verdict on Claude sonnet, recorded as no verdict") {
		t.Fatalf("node.log:\n%s", logs.String())
	}
}

// A session with no VERDICT line on a platform without the outcome
// records nothing either, and the lack is said once across the two.
func TestNoVerdictLineOnAnOlderPlatformRecordsNothing(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "I ran out of ideas.", "VERDICT: PASS")
	f.NoNoVerdict = true
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	ev.NoVerdict = &OncePer{}
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ev.Evaluate(context.Background(), *item); err == nil || !strings.Contains(err.Error(), "reached no verdict") {
			t.Fatalf("Evaluate = %v, want no verdict", err)
		}
	}
	if len(claude.jobs) != 2 || codex.runs() != 0 || len(f.CallsNamed("evaluateWorkItem")) != 0 || f.Item(item1)["status"] != "evaluating" {
		t.Fatalf("runs claude %d codex %d, verdicts %d, status %v", len(claude.jobs), codex.runs(), len(f.CallsNamed("evaluateWorkItem")), f.Item(item1)["status"])
	}
	if n := strings.Count(logs.String(), "the platform takes no no-verdict outcome"); n != 1 {
		t.Fatalf("said %d times:\n%s", n, logs.String())
	}
}

// The pass in the record (ADR 011's amendment 2026-09-29): Codex saw no
// context on the first attempt, Claude evaluated it and passed a high
// goal's item; the second opinion is not asked of Codex again — it
// could not reach the context a moment ago, and asked again it failed
// the item on what it could not see — so the pass stands, passed=true.
func TestBlindFirstAttemptThenPassRecordsPassed(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "high", "sonnet", "Holds.\nVERDICT: PASS", "A clause does not hold.\nVERDICT: FAIL")
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	codex.tools = noContext(0, "prologue: not found")
	codex.firstOnly = true
	claude.tools = map[string]*harness.ToolStat{"mesh-context": {Calls: 2}}
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 {
		t.Fatalf("runs: claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	if in["passed"] != true {
		t.Fatalf("recorded %+v", in)
	}
	notes, _ := in["notes"].(string)
	for _, want := range []string{
		"Codex (first attempt, gpt-6-luna): no verdict — mesh-context: prologue: not found\n\nHolds.",
		"Codex second opinion unavailable: it saw no context on the first attempt (mesh-context: prologue: not found)",
	} {
		if !strings.Contains(notes, want) {
			t.Fatalf("notes lack %q:\n%s", want, notes)
		}
	}
}

// A session that returns a verdict records it exactly as before: PASS
// is passed=true, FAIL passed=false, and neither carries an outcome.
func TestAVerdictIsRecordedAsBefore(t *testing.T) {
	for _, tc := range []struct {
		says   string
		passed bool
	}{{"Holds.\nVERDICT: PASS", true}, {"Nothing asserts it.\nVERDICT: FAIL", false}} {
		f, ev, _, _, _, _ := familyFixture(t, "normal", "sonnet", tc.says, "")
		ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
		in := evaluateItem1(t, f, ev)
		if _, sent := in["outcome"]; sent || in["passed"] != tc.passed || in[gateway.NoVerdictField] == true {
			t.Fatalf("%q recorded %+v", tc.says, in)
		}
	}
}

// A Claude session whose context server was connected at startup but
// whose every call Claude Code refused before it reached the server (a
// permission refusal) saw no context: its verdict is discarded, parsed
// from the stream as the harness reads it, and with no other family it
// is recorded as no verdict, never kept as an answer.
func TestRefusedCallsOnAConnectedContextServerAreNoVerdict(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", "VERDICT: PASS")
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	stream := `{"type":"system","subtype":"init","model":"claude-sonnet-5","mcp_servers":[{"name":"mesh-context","status":"connected"}]}
{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"mcp__mesh-context__mesh_document","input":{"slug":"prd"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"Claude requested permissions to use mcp__mesh-context__mesh_document, but you haven't granted it yet."}]}}
{"type":"result","subtype":"success","result":"VERDICT: FAIL","total_cost_usd":0.1,"num_turns":1}`
	claude.tools = harness.ParseStream(strings.NewReader(stream), nil).Tools
	ev.Blind = &OncePer{}
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 1 || codex.runs() != 0 {
		t.Fatalf("runs claude %d codex %d", len(claude.jobs), codex.runs())
	}
	if !sentNoVerdict(in) {
		t.Fatalf("recorded %+v", in)
	}
	if notes, _ := in["notes"].(string); !strings.Contains(notes, "saw no context (mesh-context: Claude requested permissions") {
		t.Fatalf("notes %q", notes)
	}
}

// A context server that answered some calls saw the context: the
// verdict stands as before.
func TestSomeContextIsAVerdict(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "VERDICT: PASS", "Nothing asserts it.\nVERDICT: FAIL")
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	codex.tools = map[string]*harness.ToolStat{"mesh-context": {Calls: 3, Clean: 2, Errors: 1, FirstError: "timeout"}}
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 0 || in["passed"] != false || in["notes"] != "Nothing asserts it.\n\n(evaluated on Codex gpt-6-luna)\ntools: mesh-context: first call errored: timeout" {
		t.Fatalf("runs claude %d codex %d, recorded %+v", len(claude.jobs), codex.runs(), in)
	}
}

// An evaluation an older node started names the built-in by its old
// name, prologue; adopted, its broken context is still no verdict
// (tools.IsBuiltin, an alias read until 2026-10-28).
func TestNoContextUnderTheOldNameIsNotAVerdict(t *testing.T) {
	f, ev, _, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", "VERDICT: PASS")
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(context.Background(), ev.GW, *item)
	if err != nil {
		t.Fatal(err)
	}
	// adopted: no account and no second job, as AdoptEvaluation runs it
	run := evalRun{item: *item, res: res, label: "ship-it/1", model: "sonnet", fam: family{model: "sonnet", claude: "sonnet"}, dir: t.TempDir(), adopted: true}
	result := harness.Result{Text: "VERDICT: PASS", Tools: map[string]*harness.ToolStat{"prologue": {Calls: 2, Errors: 2, FirstError: "Auth required"}}}
	if err := ev.afterEvaluate(context.Background(), run, result, nil); err != nil {
		t.Fatalf("afterEvaluate = %v", err)
	}
	calls := f.CallsNamed("evaluateWorkItem")
	if codex.runs() != 0 || len(calls) != 1 || !sentNoVerdict(calls[0].Input) {
		t.Fatalf("codex runs %d, verdicts %+v", codex.runs(), calls)
	}
	if !strings.Contains(logs.String(), "saw no context (prologue: Auth required); not a verdict") {
		t.Fatalf("node.log:\n%s", logs.String())
	}
}

// A session this node started was rendered mesh-context, so a prologue
// in its result is an ordinary console tool, not the built-in's old
// name: every call of it failing while mesh-context works leaves the
// verdict standing (the alias, read until 2026-10-28, is for adopted
// sessions only).
func TestAConsoleToolNamedPrologueFailingIsNotNoContext(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: PASS", "VERDICT: FAIL")
	ev.MCP = MeshContextMCP(f.URL(), "rs_test")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	claude.tools = map[string]*harness.ToolStat{
		"mesh-context": {Calls: 3},
		"prologue":     {Calls: 2, Errors: 2, FirstError: "upstream down"},
	}
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 0 || len(claude.jobs) != 1 || in["passed"] != true {
		t.Fatalf("runs claude %d codex %d, recorded %+v", len(claude.jobs), codex.runs(), in)
	}
	if strings.Contains(logs.String(), "saw no context") {
		t.Fatalf("node.log:\n%s", logs.String())
	}
}

const bwrapLine = "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted"

// An unpinned Codex evaluation whose preflight's sandbox probe fails
// runs on Claude, and node.log and the notes name the reason.
func TestCodexSandboxPreflightEvaluatesOnClaude(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: PASS")
	ev.CodexPreflight = (&fakePreflight{err: errors.New("sandbox: " + bwrapLine)}).check
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 0 || len(claude.jobs) != 1 {
		t.Fatalf("runs: claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	if notes, _ := in["notes"].(string); in["passed"] != true || !strings.Contains(notes, "the Codex preflight failed: sandbox: "+bwrapLine) {
		t.Fatalf("recorded %+v", in)
	}
	if want := "item ship-it/1: Codex preflight failed (sandbox: " + bwrapLine + "); evaluating on Claude"; !strings.Contains(logs.String(), want) {
		t.Fatalf("node.log lacks %q:\n%s", want, logs.String())
	}
}

// A Codex session whose sandbox blocked every command wrote VERDICT:
// PASS anyway: that is not a verdict. The same job runs on Claude and
// Claude's verdict is the one recorded, the Codex attempt first in the
// notes.
func TestCodexSandboxBrokenIsNotAVerdict(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "It fails its check.\nVERDICT: FAIL", "Read the diff.\nVERDICT: PASS")
	codex.sandbox = bwrapLine
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 1 || len(claude.jobs) != 1 || codex.jobs[0].Prompt != claude.jobs[0].Prompt {
		t.Fatalf("runs: claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	want := "Codex (first attempt, gpt-6-luna): no verdict — sandbox: " + bwrapLine + "\n\nIt fails its check.\n\n(evaluated on sonnet)"
	if in["passed"] != false || in["notes"] != want {
		t.Fatalf("recorded %+v\nwant notes %q", in, want)
	}
	if s := "item ship-it/1: evaluation on Codex gpt-6-luna ran in a sandbox that blocked every command (sandbox: " + bwrapLine + "); not a verdict, evaluating on Claude sonnet"; !strings.Contains(logs.String(), s) {
		t.Errorf("node.log lacks %q:\n%s", s, logs.String())
	}
}

// With no other family — the item pins Codex — a sandbox-blocked
// session on a platform that takes no no-verdict outcome records
// nothing: the item stays in evaluating, said once.
func TestCodexSandboxBrokenWithNoOtherFamilyWaits(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", "VERDICT: PASS")
	f.NoNoVerdict = true
	f.WorkItems[item1]["evaluateModel"] = "gpt-6-astra"
	codex.sandbox = bwrapLine
	ev.Blind, ev.NoVerdict = &OncePer{}, &OncePer{}
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ev.Evaluate(context.Background(), *item); !errors.Is(err, ErrNoContext) {
			t.Fatalf("Evaluate = %v, want ErrNoContext", err)
		}
	}
	if len(claude.jobs) != 0 || codex.runs() != 2 || len(f.CallsNamed("evaluateWorkItem")) != 0 || f.Item(item1)["status"] != "evaluating" {
		t.Fatalf("runs claude %d codex %d, verdicts %d, status %v", len(claude.jobs), codex.runs(), len(f.CallsNamed("evaluateWorkItem")), f.Item(item1)["status"])
	}
	line := "item ship-it/1: evaluation on Codex gpt-6-astra ran in a sandbox that blocked every command (sandbox: " + bwrapLine + "); not a verdict, and Claude cannot evaluate it: the item pins Codex gpt-6-astra; it stays in evaluating"
	if n := strings.Count(logs.String(), line); n != 1 {
		t.Fatalf("said %d times:\n%s", n, logs.String())
	}
}

// The sandbox would not start — bubblewrap's loopback refused, the
// research note's `bwrap: loopback: Failed RTM_NEWADDR` — and the item
// pins Codex, so no other family can evaluate it: on a platform that
// takes `noVerdict` it is recorded as no verdict with the bwrap line as
// its reason, never an ordinary fail, whatever VERDICT the session wrote;
// the item stays in evaluating with its attempts unchanged (ADR 011,
// amendment 2026-09-29; runsheet ADR 035).
func TestCodexSandboxWillNotStartWithNoOtherFamilyIsNoVerdict(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: PASS", "Nothing would run.\nVERDICT: FAIL")
	f.WorkItems[item1]["evaluateModel"] = "gpt-6-astra"
	codex.sandbox = bwrapLine
	ev.Blind, ev.NoVerdict = &OncePer{}, &OncePer{}
	attempts := f.Item(item1)["attempts"]
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 0 || codex.runs() != 1 {
		t.Fatalf("runs claude %d codex %d", len(claude.jobs), codex.runs())
	}
	reason := "evaluation on Codex gpt-6-astra ran in a sandbox that blocked every command (sandbox: " + bwrapLine + "); not a verdict, and Claude cannot evaluate it: the item pins Codex gpt-6-astra"
	if !sentNoVerdict(in) || in[gateway.NoVerdictReasonField] != reason || in["notes"] != reason {
		t.Fatalf("recorded %+v\nwant reason %q", in, reason)
	}
	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) != 1 {
		t.Fatalf("evaluateWorkItem sent %d times", len(calls))
	}
	if c := calls[0].Input; c["passed"] != false || c[gateway.NoVerdictField] != true {
		t.Fatalf("recorded as a fail: %+v", c)
	}
	if _, ok := in["outcome"]; ok {
		t.Fatalf("sent outcome: %+v", in)
	}
	item := f.Item(item1)
	if item["status"] != "evaluating" || item["attempts"] != attempts || item["noVerdicts"] != 1 || item["noVerdictReason"] != reason {
		t.Fatalf("item %+v", item)
	}
	if line := "item ship-it/1: evaluation reached no verdict on Codex gpt-6-astra, recorded as no verdict"; !strings.Contains(logs.String(), line) {
		t.Fatalf("node.log lacks %q:\n%s", line, logs.String())
	}
	if strings.Contains(logs.String(), "the platform takes no no-verdict outcome") {
		t.Fatalf("node.log:\n%s", logs.String())
	}
}

// Claude first on an urgent item, its PASS standing: a Codex second
// opinion whose sandbox blocked every command is unavailable, whatever
// it wrote.
func TestCodexSecondOpinionSandboxBrokenIsUnavailable(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "urgent", "opus", "Holds.\nVERDICT: PASS", "Blocked.\nVERDICT: FAIL")
	f.WorkItems[item1]["evaluateModel"] = "claude-opus-5"
	codex.sandbox = bwrapLine
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 1 || codex.runs() != 1 {
		t.Fatalf("runs: claude %d, codex %d", len(claude.jobs), codex.runs())
	}
	if notes, _ := in["notes"].(string); in["passed"] != true || !strings.Contains(notes, "Codex second opinion unavailable: sandbox broken ("+bwrapLine+")") {
		t.Fatalf("recorded %+v", in)
	}
}

// A Codex evaluation whose sandbox refused every test's socket
// (Result.SandboxBroken carrying the socket line) with no other family
// to run on is recorded as no verdict with that line as its reason —
// never evaluateWorkItem with passed false, whatever VERDICT it wrote
// (ADR 011, amendment 2026-09-29).
func TestEvaluationOnCodexWithNoNetworkIsNoVerdict(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "VERDICT: PASS", "The tests would not run.\nVERDICT: FAIL")
	f.WorkItems[item1]["evaluateModel"] = "gpt-6-astra"
	socket := "panic: httptest: failed to listen on a port: listen tcp6 [::1]:0: socket: operation not permitted"
	codex.sandbox = socket
	ev.Blind, ev.NoVerdict = &OncePer{}, &OncePer{}
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 0 || codex.runs() != 1 {
		t.Fatalf("runs claude %d codex %d", len(claude.jobs), codex.runs())
	}
	notes, _ := in["notes"].(string)
	if !sentNoVerdict(in) || !strings.Contains(notes, "sandbox: "+socket) {
		t.Fatalf("recorded %+v", in)
	}
	for _, c := range f.CallsNamed("evaluateWorkItem") {
		if c.Input["passed"] == false && c.Input[gateway.NoVerdictField] != true {
			t.Fatalf("recorded as a fail: %+v", c.Input)
		}
	}
}

// The streams the socket rule once let through — a search naming
// `go test` that passed, a test Codex declined or that reported no exit
// code, beside a socket-refused `go test` — parse as SandboxBroken and
// are recorded as no verdict, never a fail (ADR 011, amendment
// 2026-09-29).
func TestEvaluationOnCodexWithTestsUnrunIsNoVerdict(t *testing.T) {
	for _, name := range []string{"codex_exec_socket_grep.jsonl", "codex_exec_socket_declined.jsonl"} {
		t.Run(name, func(t *testing.T) {
			stream, err := os.Open(filepath.Join("..", "harness", "testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			res := harness.ParseCodexStream(stream, "gpt-6-sol", nil)
			if res.SandboxBroken == "" {
				t.Fatalf("%s parsed with no SandboxBroken", name)
			}
			f, ev, _, codex, _, _ := familyFixture(t, "normal", "sonnet", "VERDICT: PASS", res.Text)
			f.WorkItems[item1]["evaluateModel"] = "gpt-6-sol"
			codex.sandbox = res.SandboxBroken
			ev.Blind, ev.NoVerdict = &OncePer{}, &OncePer{}
			in := evaluateItem1(t, f, ev)
			notes, _ := in["notes"].(string)
			if !sentNoVerdict(in) || !strings.Contains(notes, "socket: operation not permitted") {
				t.Fatalf("recorded %+v", in)
			}
			for _, c := range f.CallsNamed("evaluateWorkItem") {
				if c.Input["passed"] == false && c.Input[gateway.NoVerdictField] != true {
					t.Fatalf("recorded as a fail: %+v", c.Input)
				}
			}
		})
	}
}

// The mini's 04:00:22Z exit (2026-09-29, goal
// node-yaml-takes-effect-or-says-why-not): a Codex preflight fails, the
// evaluation turns to Claude and the Claude account cannot be opened —
// that minute a 429, here no Claude credential issued. Open's nil
// account overwrote acct, and the deferred acct.Cleanup() dereferenced
// it: a nil-pointer panic that took the node down with exit status 2.
// Now the error comes back and the Codex account is cleaned up once.
func TestCodexPreflightFailureWithNoClaudeAccountReturnsTheError(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: FAIL")
	f.SessionCredentials[item1] = f.SessionCredentials[item1][1:]
	ev.CodexPreflight = (&fakePreflight{err: errors.New("initialize: 429 Too Many Requests: Rate exceeded.")}).check
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	err = ev.Evaluate(context.Background(), *item)
	if !errors.Is(err, ErrNoAccount) {
		t.Fatalf("Evaluate: %v, want ErrNoAccount", err)
	}
	if len(claude.jobs) != 0 || codex.runs() != 0 || len(f.CallsNamed("evaluateWorkItem")) != 0 {
		t.Fatalf("runs claude %d, codex %d; evaluate calls %d", len(claude.jobs), codex.runs(), len(f.CallsNamed("evaluateWorkItem")))
	}
	if want := "item ship-it/1: Codex preflight failed (initialize: 429 Too Many Requests: Rate exceeded.); evaluating on Claude"; !strings.Contains(logs.String(), want) {
		t.Fatalf("node.log lacks %q:\n%s", want, logs.String())
	}
}

// The goroutine the 04:00:22Z panic ran on — evaluateHeld's — is
// guarded: a panicking evaluation is the whole fatal line and exit
// crash.ExitPanic, not the runtime's bare status 2 (a recover on the
// start path alone never sees another goroutine's panic).
func TestPanickingEvaluationNamesItself(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	f.WorkItems[item1]["status"] = "evaluating"
	f.WorkItems[item1]["nodeId"] = cfg.NodeID
	var out syncBuf
	status := make(chan int, 1)
	was, exit := crash.Out, crash.Exit
	crash.Out, crash.Exit = &out, func(code int) { status <- code }
	t.Cleanup(func() { crash.Out, crash.Exit = was, exit })
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Limits: &Limits{}, Log: log.New(&syncBuf{}, "", 0),
		Evaluate: func(context.Context, gateway.WorkItem) {
			var acct *SessionAccount
			acct.Cleanup()
		}}
	loop.evaluateHeld(context.Background())
	loop.wg.Wait()
	if code := <-status; code != crash.ExitPanic {
		t.Fatalf("exit %d, want %d", code, crash.ExitPanic)
	}
	first, _, _ := strings.Cut(out.String(), "\n")
	if want := "mesh node: fatal: runtime error: invalid memory address or nil pointer dereference (version " + version.Resolved() + ")"; first != want {
		t.Fatalf("first line %q, want %q", first, want)
	}
}
