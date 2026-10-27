package node

import (
	"context"
	"errors"
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
	"projectmesh.io/mesh-cli/internal/tools"
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
		Codex: ObserveLimits(codex, limits), CodexModel: CodexConfig{}.CodexModel,
		CodexAccounts: loop.CodexCandidates, Waits: &OncePer{}, Limits: limits, Thresholds: func() LimitsConfig { return LimitsConfig{} },
		Credentials: &Credentials{GW: gw, Limits: limits, Known: known, Log: log.New(logs, "", 0)}, Log: log.New(logs, "", 0)}
	return f, ev, claude, codex, limits, logs
}

// usageOf is the one usage report evaluateWorkItem carried.
func usageOf(t *testing.T, f *gatewaytest.Fake) map[string]any {
	t.Helper()
	fin, _ := evaluationClosed(t, f)["finish"].(map[string]any)
	res, _ := fin["result"].(map[string]any)
	u, ok := res["usage"].(map[string]any)
	if !ok {
		t.Fatalf("the evaluate job closed with no usage: %+v", res)
	}
	return u
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
	if in["notes"] != "(evaluated on Codex gpt-6-luna)" || in["costCents"] != float64(40) {
		t.Fatalf("recorded %+v", in)
	}
	u := usageOf(t, f)
	if u["adapter"] != "codex" || u["model"] != "gpt-6-luna" || u["credentialId"] != "oa-1" || u["credentialKind"] != "subscription" || u["inputTokens"] != float64(1000) {
		t.Fatalf("usage: %v", u)
	}
	for _, want := range []string{"item ship-it/1: evaluate on Codex gpt-6-luna: mesh/ship-it-1", "item ship-it/1: evaluation handed in through the product's tool on Codex gpt-6-luna"} {
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
	if in["notes"] != "(evaluated on Claude sonnet)" {
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
		_ = evaluateItem1(t, f, ev)
		if len(claude.jobs) != 1 || codex.runs() != 0 || claude.jobs[0].Model != "claude-opus-5" {
			t.Fatalf("%v: runs claude %d, codex %d", pin, len(claude.jobs), codex.runs())
		}
		if u := usageOf(t, f); u["adapter"] != "claude-code" {
			t.Fatalf("%v: usage %v", pin, u)
		}
	}
}

// A node with no Codex account known and no codex: block evaluates on
// Claude as it always has, even with the Codex harness built: no Codex
// run and no second-opinion line.
func TestNoCodexAccountEvaluatesAsBefore(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "urgent", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: FAIL")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 1 || codex.runs() != 0 || in["notes"] != "(evaluated on Claude sonnet)" {
		t.Fatalf("runs claude %d codex %d, recorded %+v", len(claude.jobs), codex.runs(), in)
	}
}

// The loop starts an evaluation no Claude account has room for when a
// Codex account has room for it (Loop.CodexRoom), and counts it as
// waiting otherwise.
func TestEvaluationStartsOnCodexRoom(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(0)}
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
	knowAccounts(loop, AccountConfig{Label: "max", Kind: AccountSubscription})
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
	if in := evaluationClosed(t, f); in["notes"] != "(evaluated on Codex gpt-6-astra)" {
		t.Fatalf("recorded %+v", in)
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
	return map[string]*harness.ToolStat{"runsheet": {Calls: calls, Errors: errs, Wall: 30 * time.Millisecond, FirstError: why}}
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

// A session this node started was rendered runsheet, so the alias
// (tools.BuiltinAlias) in its result is an ordinary console tool, not the built-in's old
// name: every call of it failing while runsheet works leaves the
// verdict standing (the alias, read until 2026-10-28, is for adopted
// sessions only).
func TestAConsoleToolNamedAsTheAliasFailingIsNotNoContext(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: PASS", "VERDICT: FAIL")
	ev.MCP = MeshContextHTTP(f.URL(), "rs_test")
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	claude.tools = map[string]*harness.ToolStat{
		"runsheet":           {Calls: 3},
		tools.BuiltinAlias(): {Calls: 2, Errors: 2, FirstError: "upstream down"},
	}
	in := evaluateItem1(t, f, ev)
	if codex.runs() != 0 || len(claude.jobs) != 1 {
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
	if notes, _ := in["notes"].(string); !strings.Contains(notes, "the Codex preflight failed: sandbox: "+bwrapLine) {
		t.Fatalf("recorded %+v", in)
	}
	if want := "item ship-it/1: Codex preflight failed (sandbox: " + bwrapLine + "); evaluating on Claude"; !strings.Contains(logs.String(), want) {
		t.Fatalf("node.log lacks %q:\n%s", want, logs.String())
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
	if !isNoCredential(err) {
		t.Fatalf("Evaluate: %v, want ErrNoCredential", err)
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

// A node that knows no issued Codex credential evaluates an unpinned
// item on Claude, even with the Codex harness built and a login in the
// operator's own $CODEX_HOME: that login is never a candidate (ADR 011's
// amendment 2026-10-02), and the report names the Claude credential.
func TestNoIssuedCodexCredentialEvaluatesOnClaude(t *testing.T) {
	f, ev, claude, codex, limits, _ := familyFixture(t, "urgent", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: FAIL")
	operator := t.TempDir()
	t.Setenv("CODEX_HOME", operator)
	if err := os.WriteFile(filepath.Join(operator, "auth.json"), []byte(codexLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	f.SessionCredentials[item1] = f.SessionCredentials[item1][:1] // the Claude one alone
	known := &KnownAccounts{}
	ev.CodexAccounts, ev.Credentials.Known = (&Loop{Known: known}).CodexCandidates, known
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 1 || codex.runs() != 0 || in["notes"] != "(evaluated on Claude sonnet)" {
		t.Fatalf("runs claude %d codex %d, recorded %+v", len(claude.jobs), codex.runs(), in)
	}
	if u := usageOf(t, f); u["adapter"] != "claude-code" || u["credentialId"] != "cred-1" {
		t.Fatalf("usage %v", u)
	}
	if got := limits.CodexReadings(); len(got) != 0 {
		t.Errorf("Codex readings %v, want none: no Codex session ran", got)
	}
}

// An item whose only Codex account is gone — Known still holds it, but
// the claim-time read issues no Codex credential now — runs on Claude,
// said in node.log; a pinned gpt item waits in evaluating instead.
func TestCodexAccountGoneFamilyFallsBackOrWaits(t *testing.T) {
	f, ev, claude, codex, _, logs := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: FAIL")
	f.SessionCredentials[item1] = f.SessionCredentials[item1][:1]
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 1 || codex.runs() != 0 {
		t.Fatalf("unpinned: runs claude %d codex %d, recorded %+v", len(claude.jobs), codex.runs(), in)
	}
	if !strings.Contains(logs.String(), "no issued Codex credential has headroom after all; evaluating on Claude") {
		t.Errorf("node.log:\n%s", logs.String())
	}

	f, ev, claude, codex, _, _ = familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: FAIL")
	f.SessionCredentials[item1] = f.SessionCredentials[item1][:1]
	f.WorkItems[item1]["evaluateModel"] = "gpt-6-sol"
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); !errors.Is(err, ErrFamilyWaits) {
		t.Fatalf("pinned: Evaluate = %v, want ErrFamilyWaits", err)
	}
	if len(claude.jobs) != 0 || codex.runs() != 0 || len(f.CallsNamed("evaluateWorkItem")) != 0 {
		t.Fatalf("pinned: runs claude %d codex %d", len(claude.jobs), codex.runs())
	}
}

// A pinned gpt item on a node that knows no issued Codex credential at
// all waits in evaluating: nothing runs, on Codex or on Claude.
func TestCodexPinWaitsWithNoIssuedCredential(t *testing.T) {
	f, ev, claude, codex, _, _ := familyFixture(t, "high", "sonnet", "VERDICT: PASS", "VERDICT: PASS")
	f.WorkItems[item1]["evaluateModel"] = "gpt-6-sol"
	f.SessionCredentials[item1] = f.SessionCredentials[item1][:1]
	ev.CodexAccounts = (&Loop{Known: &KnownAccounts{}}).CodexCandidates
	item, err := ev.GW.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.Evaluate(context.Background(), *item); !errors.Is(err, ErrFamilyWaits) {
		t.Fatalf("Evaluate = %v, want ErrFamilyWaits", err)
	}
	if len(claude.jobs) != 0 || codex.runs() != 0 || f.Item(item1)["status"] != "evaluating" {
		t.Fatalf("runs claude %d codex %d, status %v", len(claude.jobs), codex.runs(), f.Item(item1)["status"])
	}
}

// Every Codex evaluation is opened through Credentials.Open(adapter
// codex) on an issued credential, so its usage report always carries a
// credentialId and a credentialKind of subscription or api — never
// unset, which only the operator's own login ever reported.
func TestCodexEvaluationUsageCarriesItsCredential(t *testing.T) {
	for _, tc := range []struct {
		cred gateway.Credential
		raw  map[string]any
		kind string
	}{
		{codexSub(), map[string]any{"id": "oa-1", "label": "chatgpt-pro", "vendor": "openai", "kind": "oauth_token", "serves": []any{"codex"}, "secret": codexLogin, "secretVersion": float64(1)}, "subscription"},
		{codexAPI(), map[string]any{"id": "oa-2", "label": "openai-key", "vendor": "openai", "kind": "api_key", "serves": []any{"codex"}, "secret": codexKey, "secretVersion": float64(1), "capUnit": "dollars", "dailyCap": 30.0}, "api"},
	} {
		f, ev, claude, codex, _, _ := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", "Holds.\nVERDICT: PASS")
		f.SessionCredentials[item1] = []map[string]any{f.SessionCredentials[item1][0], tc.raw}
		known := &KnownAccounts{}
		if err := known.Note(IssuedAccount(tc.cred), time.Now()); err != nil {
			t.Fatal(err)
		}
		ev.CodexAccounts, ev.Credentials.Known = (&Loop{Known: known}).CodexCandidates, known
		evaluateItem1(t, f, ev)
		if len(claude.jobs) != 0 || codex.runs() != 1 {
			t.Fatalf("%s: runs claude %d codex %d", tc.cred.Label, len(claude.jobs), codex.runs())
		}
		u := usageOf(t, f)
		if u["adapter"] != "codex" || u["credentialId"] != tc.cred.ID || u["credentialKind"] != tc.kind {
			t.Fatalf("%s: usage %v", tc.cred.Label, u)
		}
	}
}

// A Codex account with headroom but a preflight that fails: the
// unpinned item is evaluated on Claude, the job's notes tail says so
// and why, and node.log names the reason; nothing ran on Codex.
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
	want := "(evaluated on Claude sonnet; the Codex preflight failed: refuses auth: 401 Unauthorized: bad token)"
	if in["notes"] != want {
		t.Fatalf("recorded %+v\nwant notes %q", in, want)
	}
	if u := usageOf(t, f); u["adapter"] != "claude-code" || u["credentialId"] != "cred-1" {
		t.Fatalf("usage %v", u)
	}
	if want := "item ship-it/1: Codex preflight failed (refuses auth: 401 Unauthorized: bad token); evaluating on Claude"; !strings.Contains(logs.String(), want) {
		t.Fatalf("node.log lacks %q:\n%s", want, logs.String())
	}
}
