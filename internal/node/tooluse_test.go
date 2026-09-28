package node

import (
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/tools"
)

const (
	braveSecret      = "brave-key-7c1d93"
	perplexitySecret = "pplx-key-2b8e51"
)

// braveTool and perplexityTool are the claim-time read's rows for two
// remote tools, each with its key as a secret header.
func braveTool() map[string]any {
	return map[string]any{"id": "t-brave", "slug": "brave", "name": "Brave", "kind": "remote_mcp", "need": "nice_to_have", "url": "https://brave.example/mcp",
		"account": map[string]any{"id": "a-brave", "label": "brave"},
		"headers": []any{map[string]any{"name": "X-Subscription-Token", "value": braveSecret}}, "env": []any{}}
}

func perplexityTool() map[string]any {
	return map[string]any{"id": "t-pplx", "slug": "perplexity", "name": "Perplexity", "kind": "remote_mcp", "need": "nice_to_have", "url": "https://perplexity.example/mcp",
		"account": map[string]any{"id": "a-pplx", "label": "perplexity"},
		"headers": []any{map[string]any{"name": "Authorization", "value": "Bearer " + perplexitySecret}}, "env": []any{}}
}

// braveErrored is a session's three brave calls, the first of which
// errored with a message echoing the key it was sent, and none for
// perplexity.
func braveErrored() (map[string]int, map[string]*harness.ToolStat) {
	return map[string]int{"brave": 3, "prologue": 2},
		map[string]*harness.ToolStat{
			"brave":    {Calls: 3, Errors: 1, Wall: 1500 * time.Millisecond, FirstError: "401 Unauthorized: token " + braveSecret + " is not valid"},
			"prologue": {Calls: 2, Wall: 40 * time.Millisecond},
		}
}

// A session served brave and perplexity whose result has three brave
// calls (one erroring) and none for perplexity: its report carries the
// per-tool rows beside the total, and its note names perplexity as never
// called and brave's first error, with no secret in it.
func TestSessionAccountReportsToolsAndNotesTheirUse(t *testing.T) {
	st := SelectTools(map[string]any{"prologue": map[string]any{}}, []tools.Tool{
		{Name: "brave", Transport: tools.Remote, URL: "https://brave.example/mcp", Secrets: map[string]string{"X-Subscription-Token": braveSecret}},
		{Name: "perplexity", Transport: tools.Remote, URL: "https://perplexity.example/mcp", Secrets: map[string]string{"Authorization": "Bearer " + perplexitySecret}},
	}, string(harness.KindBuild), nil)
	acct := &SessionAccount{creds: &Credentials{}, started: time.Now()}
	var job harness.Job
	if err := st.UseTools(&job, map[string]any{"prologue": map[string]any{}}, acct); err != nil {
		t.Fatal(err)
	}
	calls, stats := braveErrored()
	res := harness.Result{SessionID: "s-1", ToolCalls: calls, Tools: stats}

	rep := acct.Report(res)
	if rep.ToolCalls != 5 {
		t.Fatalf("toolCalls = %d", rep.ToolCalls)
	}
	want := []gateway.ToolCallReport{{Tool: "brave", Calls: 3, Errors: 1, Ms: 1500}, {Tool: "prologue", Calls: 2, Ms: 40}}
	if len(rep.Tools) != len(want) || rep.Tools[0] != want[0] || rep.Tools[1] != want[1] {
		t.Fatalf("tools = %+v", rep.Tools)
	}

	note := acct.UseNote(res)
	if note != "tools: brave: first call errored: 401 Unauthorized: token [redacted] is not valid; perplexity served, never called" {
		t.Fatalf("note = %q", note)
	}
	for _, s := range []string{braveSecret, perplexitySecret} {
		if strings.Contains(note, s) {
			t.Fatalf("secret %s in the note %q", s, note)
		}
	}
	if strings.Contains(note, "prologue") {
		t.Fatalf("the built-in named in the note %q", note)
	}

	// every served tool called cleanly: no note
	clean := harness.Result{ToolCalls: map[string]int{"brave": 1, "perplexity": 2}, Tools: map[string]*harness.ToolStat{"brave": {Calls: 1}, "perplexity": {Calls: 2}}}
	if note := acct.UseNote(clean); note != "" {
		t.Fatalf("clean note = %q", note)
	}
}

// Even redacted by nothing, a secret header on the tool never reaches
// the note, and a long error is capped at 200 characters.
func TestUseNoteRedactsTheToolsOwnValues(t *testing.T) {
	st := SessionTools{Use: []tools.Tool{{Name: "brave", Headers: map[string]string{"X-Key": braveSecret}}}}
	res := harness.Result{ToolCalls: map[string]int{"brave": 1}, Tools: map[string]*harness.ToolStat{
		"brave": {Calls: 1, Errors: 1, FirstError: braveSecret + " " + strings.Repeat("x", 400)},
	}}
	note := st.UseNote(res, nil)
	if strings.Contains(note, braveSecret) || !strings.HasPrefix(note, "tools: brave: first call errored: [redacted] xxx") {
		t.Fatalf("note = %q", note)
	}
	if n := len(strings.TrimPrefix(note, "tools: brave: first call errored: ")); n > 200+len("…")-1 {
		t.Fatalf("error text %d bytes long", n)
	}
}

// A report folds another session's tools in by name (the Codex second
// opinion), summing calls, errors and ms.
func TestReportMergesCarriedTools(t *testing.T) {
	acct := &SessionAccount{creds: &Credentials{}, started: time.Now()}
	acct.Report(harness.Result{ToolCalls: map[string]int{"brave": 3}, Tools: map[string]*harness.ToolStat{"brave": {Calls: 3, Errors: 1, Wall: time.Second}}})
	second := &SessionAccount{creds: &Credentials{}, started: time.Now()}
	second.res = harness.Result{ToolCalls: map[string]int{"brave": 1, "context7": 2}, Tools: map[string]*harness.ToolStat{
		"brave": {Calls: 1, Wall: 500 * time.Millisecond}, "context7": {Calls: 2, Errors: 2, Wall: 200 * time.Millisecond},
	}}
	acct.Carries(second)
	want := []gateway.ToolCallReport{{Tool: "brave", Calls: 4, Errors: 1, Ms: 1500}, {Tool: "context7", Calls: 2, Errors: 2, Ms: 200}}
	got := acct.report.Tools
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] || acct.report.ToolCalls != 6 {
		t.Fatalf("tools = %+v, toolCalls %d", got, acct.report.ToolCalls)
	}
}

// A build served brave and perplexity notes on the item, through
// ReportWorkItemProgress, which it never called and which errored, with
// no secret in any note; its finish carries the per-tool rows.
func TestBuildNotesItsToolUse(t *testing.T) {
	f := seed(t)
	f.SessionTools[item1] = []map[string]any{braveTool(), perplexityTool()}
	calls, stats := braveErrored()
	h := &fakeHarness{toolCalls: calls, tools: stats}
	r, item, logs := toolRunner(t, f, h)

	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	var notes []string
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		notes = append(notes, c.Input["note"].(string))
	}
	all := strings.Join(notes, "\n")
	want := "tools: brave: first call errored: 401 Unauthorized: token [redacted] is not valid; perplexity served, never called"
	found := false
	for _, n := range notes {
		found = found || n == want
	}
	if !found {
		t.Fatalf("no tool-use note in %q", notes)
	}
	for _, s := range []string{braveSecret, perplexitySecret} {
		if strings.Contains(all, s) || strings.Contains(logs.String(), s) {
			t.Fatalf("secret %s reported or logged", s)
		}
	}
	if len(f.SessionUsage) != 1 || f.SessionUsage[0].Name != "finishWorkItem" {
		t.Fatalf("session usage = %+v", f.SessionUsage)
	}
	rows, _ := f.SessionUsage[0].Input["tools"].([]any)
	if len(rows) != 2 {
		t.Fatalf("tools = %#v", f.SessionUsage[0].Input["tools"])
	}
	brave, _ := rows[0].(map[string]any)
	if brave["tool"] != "brave" || brave["calls"] != 3.0 || brave["errors"] != 1.0 || brave["ms"] != 1500.0 {
		t.Fatalf("brave row = %#v", brave)
	}
	if f.SessionUsage[0].Input["toolCalls"] != 5.0 {
		t.Fatalf("toolCalls = %v", f.SessionUsage[0].Input["toolCalls"])
	}
}

// An evaluation's notes carry the same tool-use note.
func TestEvaluationNotesItsToolUse(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: "22222222-2222-4222-8222-222222222222"}
	f.WorkItems[item1]["status"], f.WorkItems[item1]["nodeId"], f.WorkItems[item1]["branch"] = "evaluating", cfg.NodeID, "mesh/ship-it-1"
	var item gateway.WorkItem
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Branch = item1, orgID, goalID, repoID, 1, "mesh/ship-it-1"
	ws := &fakeWS{root: t.TempDir()}
	calls, stats := braveErrored()
	h := &fakeHarness{text: "Fine.\nVERDICT: PASS", toolCalls: calls, tools: stats}
	logs := &syncBuf{}
	ev := &Evaluator{GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MCP: PrologueMCP(f.URL(), "rs_test"),
		Credentials: &Credentials{GW: gw}, Runtimes: onlyNpx, Declines: &Declines{}, Log: log.New(logs, "", 0)}
	f.SessionTools[item1] = []map[string]any{braveTool(), perplexityTool()}

	if err := ev.Evaluate(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	notes, _ := f.CallsNamed("evaluateWorkItem")[0].Input["notes"].(string)
	if notes != "Fine.\n\n(evaluated on the CLI's default model)\ntools: brave: first call errored: 401 Unauthorized: token [redacted] is not valid; perplexity served, never called" {
		t.Fatalf("notes = %q", notes)
	}
	if strings.Contains(logs.String(), braveSecret) {
		t.Fatal("secret in the evaluator's log")
	}
}

// Against a platform whose usage report has no `tools`, the build still
// finishes with the report's total, and the node says once that it
// logs the per-tool calls instead, and logs them.
func TestBuildOnAPlatformWithoutToolUsage(t *testing.T) {
	f := seed(t)
	f.NoUsageTools = true
	f.SessionTools[item1] = []map[string]any{braveTool(), perplexityTool()}
	calls, stats := braveErrored()
	h := &fakeHarness{toolCalls: calls, tools: stats}
	r, item, logs := toolRunner(t, f, h)

	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if f.Item(item1)["status"] != "evaluating" {
		t.Fatalf("item: %v", f.Item(item1))
	}
	if len(f.SessionUsage) != 1 || f.SessionUsage[0].Input["toolCalls"] != 5.0 || f.SessionUsage[0].Input["tools"] != nil {
		t.Fatalf("session usage = %+v", f.SessionUsage)
	}
	out := logs.String()
	if !strings.Contains(out, "usage: the platform takes no per-tool calls yet; they are logged here instead") ||
		!strings.Contains(out, `tools: [{"tool":"brave","calls":3,"errors":1,"ms":1500}`) {
		t.Fatalf("log:\n%s", out)
	}
}

// Carried takes ErrNoToolUsage as a landed command.
func TestCarriedTakesNoToolUsage(t *testing.T) {
	acct := &SessionAccount{creds: &Credentials{}, started: time.Now()}
	acct.Report(harness.Result{})
	if err := acct.Carried(gateway.ErrNoToolUsage); err != nil {
		t.Fatal(err)
	}
	if !acct.handedIn || !acct.noToolList {
		t.Fatalf("handedIn %v, noToolList %v", acct.handedIn, acct.noToolList)
	}
	if err := acct.Carried(errors.New("boom")); err == nil {
		t.Fatal("another error was swallowed")
	}
}
