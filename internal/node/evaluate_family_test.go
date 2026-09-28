package node

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
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
