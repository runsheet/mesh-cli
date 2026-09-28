package node

import (
	"context"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/tools"
)

const (
	searchSecret = "tok-search-5e1f0b"
	fsSecret     = "key-fs-a93c77"
	gitSecret    = "key-git-0d42be"
)

// remoteTool, localTool and uvxTool are the claim-time read's rows (the
// gateway's JSON), as the platform hands them to the claiming node
// (itemTools, runsheet ADR 020): a tool served through an account has
// its settings as that account's secrets.
func remoteTool() map[string]any {
	return map[string]any{"id": "t-search", "slug": "search", "name": "Search", "kind": "remote_mcp", "need": "required", "url": "https://search.example/mcp",
		"account": map[string]any{"id": "a-search", "label": "search-team"},
		"headers": []any{map[string]any{"name": "X-Region", "value": "au"}, map[string]any{"name": "Authorization", "value": "Bearer " + searchSecret}}, "env": []any{}}
}

func localTool() map[string]any {
	return map[string]any{"id": "t-fs", "slug": "fs-tools", "name": "FS tools", "kind": "local_mcp", "need": "nice_to_have", "runtime": "npx", "command": "@example/fs-mcp",
		"args": []any{"--root", "."}, "account": map[string]any{"id": "a-fs", "label": "fs"},
		"headers": []any{}, "env": []any{map[string]any{"name": "FS_API_KEY", "value": fsSecret}}}
}

func uvxTool(required bool) map[string]any {
	need := "nice_to_have"
	if required {
		need = "required"
	}
	return map[string]any{"id": "t-git", "slug": "git-tools", "name": "Git tools", "kind": "local_mcp", "need": need, "runtime": "uvx", "command": "mcp-server-git",
		"account": map[string]any{"id": "a-git", "label": "git"},
		"headers": []any{}, "env": []any{map[string]any{"name": "GIT_KEY", "value": gitSecret}}}
}

func onlyNpx() []string { return []string{"npx"} }

// toolRunner is runnerOnFake with the claim-time read, the Prologue
// attachment, the node's runtimes and a captured log.
func toolRunner(t *testing.T, f *gatewaytest.Fake, h harness.Harness) (*Runner, gateway.WorkItem, *syncBuf) {
	t.Helper()
	r, _, item, _ := runnerOnFake(t, f, h, nil)
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	r.MCP = PrologueMCP(f.URL(), "rs_test")
	r.Credentials = &Credentials{GW: r.GW.(CredentialGateway), Log: r.Log}
	r.Runtimes = onlyNpx
	r.Declines = &Declines{}
	return r, item, logs
}

// A build whose item has a remote and a local tool gets both beside the
// built-in Prologue, every one allowed on the command line; the secret
// values are in the session's env and nowhere else — not on argv, not in
// the node's log, not in what is reported on the item.
func TestBuildGetsItsToolsBesidePrologue(t *testing.T) {
	f := seed(t)
	f.SessionTools[item1] = []map[string]any{remoteTool(), localTool()}
	h := &fakeHarness{}
	r, item, logs := toolRunner(t, f, h)

	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("%d sessions", len(h.jobs))
	}
	job := h.jobs[0]
	for _, n := range []string{"prologue", "search", "fs-tools"} {
		if job.MCPServers[n] == nil {
			t.Errorf("MCPServers has no %s: %v", n, job.MCPServers)
		}
	}
	if len(job.Tools) != 2 || job.Tools[0].Name != "search" || job.Tools[1].Name != "fs-tools" {
		t.Errorf("Tools = %+v", job.Tools)
	}
	args := (&harness.ClaudeCode{}).Args(job)
	allowed := ""
	for i, a := range args {
		if a == "--allowedTools" && i+1 < len(args) {
			allowed = args[i+1]
		}
	}
	if allowed != "mcp__fs-tools__*,mcp__prologue__*,mcp__search__*" {
		t.Errorf("--allowedTools = %q (args %v)", allowed, args)
	}
	if v, _ := envOf(job.Env, tools.EnvName("search", "Authorization")); v != searchSecret {
		t.Errorf("search's bearer in env = %q", v)
	}
	if v, _ := envOf(job.Env, tools.EnvName("fs-tools", "FS_API_KEY")); v != fsSecret {
		t.Errorf("fs-tools' key in env = %q", v)
	}
	argv := strings.Join(args, " ")
	var reported []string
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		reported = append(reported, c.Input["note"].(string))
	}
	for _, s := range []string{searchSecret, fsSecret} {
		if strings.Contains(argv, s) {
			t.Errorf("secret %s on argv", s)
		}
		if strings.Contains(logs.String(), s) {
			t.Errorf("secret %s in the node log:\n%s", s, logs.String())
		}
		if strings.Contains(strings.Join(reported, "\n"), s) {
			t.Errorf("secret %s reported on the item", s)
		}
		if !slicesContain(job.Secrets, s) {
			t.Errorf("secret %s not in Job.Secrets, so the harness would not redact it", s)
		}
	}
	if !strings.Contains(logs.String(), "tools search fs-tools") {
		t.Errorf("log names no tools:\n%s", logs.String())
	}
	if f.Item(item1)["status"] != "evaluating" {
		t.Fatalf("item: %v", f.Item(item1))
	}
}

func slicesContain(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// A nice-to-have tool whose runtime the node lacks is left out: the
// session runs without it, and the omission is noted on the item.
func TestNiceToHaveToolWithAMissingRuntimeIsOmittedAndNoted(t *testing.T) {
	f := seed(t)
	f.SessionTools[item1] = []map[string]any{localTool(), uvxTool(false)}
	h := &fakeHarness{}
	r, item, logs := toolRunner(t, f, h)

	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("%d sessions", len(h.jobs))
	}
	job := h.jobs[0]
	if job.MCPServers["git-tools"] != nil || job.MCPServers["fs-tools"] == nil || job.MCPServers["prologue"] == nil {
		t.Fatalf("MCPServers = %v", job.MCPServers)
	}
	if _, ok := envOf(job.Env, tools.EnvName("git-tools", "GIT_KEY")); ok {
		t.Error("an omitted tool's secret reached the session")
	}
	noted := false
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		if c.Input["note"] == "tools: tool git-tools omitted: this node lacks uvx" {
			noted = true
		}
	}
	if !noted {
		t.Errorf("no note of the omission: %+v", f.CallsNamed("reportWorkItemProgress"))
	}
	if strings.Contains(logs.String(), gitSecret) {
		t.Error("an omitted tool's secret in the log")
	}
	if n := len(f.CallsNamed("releaseWorkItem")); n != 0 {
		t.Fatalf("released %d times", n)
	}
}

// An item with no tools runs exactly as before: Prologue alone.
func TestItemWithNoToolsRunsOnPrologueAlone(t *testing.T) {
	f := seed(t)
	h := &fakeHarness{}
	r, item, _ := toolRunner(t, f, h)

	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	job := h.jobs[0]
	if !reflect.DeepEqual(job.MCPServers, PrologueMCP(f.URL(), "rs_test")) {
		t.Fatalf("MCPServers = %v", job.MCPServers)
	}
	if job.Tools != nil {
		t.Fatalf("Tools = %v", job.Tools)
	}
	for _, kv := range job.Env {
		if strings.HasPrefix(kv, "MESH_TOOL_") {
			t.Fatalf("tool env without tools: %s", kv)
		}
	}
	args := (&harness.ClaudeCode{}).Args(job)
	if got := args[len(args)-1]; got != "mcp__prologue__*" {
		t.Fatalf("--allowedTools = %q", got)
	}
}

// A required tool whose runtime the node lacks declines the claim: the
// item is released with a reason naming the tool and the runtime, no
// session starts, and the next tick does not claim it again — until the
// node has the runtime.
func TestRequiredToolWithAMissingRuntimeReleasesAndIsNotReclaimed(t *testing.T) {
	f := seed(t)
	f.SessionTools[item1] = []map[string]any{remoteTool(), uvxTool(true)}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(2)}
	if err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node", Harnesses: []string{"claude-code"}, Capacity: 2, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{}
	logs := &syncBuf{}
	logger := log.New(logs, "", 0)
	onPath := map[string]bool{"npx": true, "claude": true}
	lookPath := func(file string) (string, error) {
		if onPath[file] {
			return "/usr/bin/" + file, nil
		}
		return "", os.ErrNotExist
	}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: logger,
		LookPath: lookPath, Declines: &Declines{}}
	runner := &Runner{GW: gw, Harness: h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour, Log: logger,
		MCP: PrologueMCP(f.URL(), "rs_test"), Credentials: &Credentials{GW: gw, Log: logger},
		Runtimes: loop.ToolRuntimes, Declines: loop.Declines}
	loop.Dispatch = func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	waitFor(t, func() bool { return len(f.CallsNamed("releaseWorkItem")) == 1 && loop.Running() == 0 })

	rel := f.CallsNamed("releaseWorkItem")[0].Input
	if rel["aggregateId"] != item1 || rel["reason"] != "tools: required tool git-tools needs uvx, which this node lacks" {
		t.Fatalf("release: %+v", rel)
	}
	if len(h.jobs) != 0 {
		t.Fatalf("%d sessions started", len(h.jobs))
	}
	if row := f.Item(item1); row["status"] != "ready" || len(f.CallsNamed("failWorkItem")) != 0 {
		t.Fatalf("item: %v", row)
	}
	for _, s := range []string{searchSecret, gitSecret} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("secret %s in the node log", s)
		}
	}

	// the next tick, the runtime still missing: not claimed again
	loop.Tick(ctx)
	loop.wg.Wait()
	if n := len(f.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("claimWorkItem called %d times, want 1", n)
	}

	// uvx installed: the next tick takes the item and it runs with both
	onPath["uvx"] = true
	loop.Tick(ctx)
	waitFor(t, func() bool { return f.Item(item1)["status"] == "evaluating" })
	if n := len(f.CallsNamed("claimWorkItem")); n != 2 {
		t.Fatalf("claimWorkItem called %d times, want 2", n)
	}
	if len(h.jobs) != 1 || h.jobs[0].MCPServers["git-tools"] == nil || h.jobs[0].MCPServers["search"] == nil {
		t.Fatalf("jobs: %+v", h.jobs)
	}
}

// An evaluator whose required tool needs a runtime the node lacks
// records no verdict (an item in evaluating is not released) and is
// remembered as declined; a nice-to-have one is left out and named in
// the verdict's notes.
func TestEvaluatorTools(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: "22222222-2222-4222-8222-222222222222"}
	f.WorkItems[item1]["status"], f.WorkItems[item1]["nodeId"], f.WorkItems[item1]["branch"] = "evaluating", cfg.NodeID, "mesh/ship-it-1"
	var item gateway.WorkItem
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Branch = item1, orgID, goalID, repoID, 1, "mesh/ship-it-1"
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "Fine.\nVERDICT: PASS"}
	logs := &syncBuf{}
	ev := &Evaluator{GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MCP: PrologueMCP(f.URL(), "rs_test"),
		Credentials: &Credentials{GW: gw}, Runtimes: onlyNpx, Declines: &Declines{}, Log: log.New(logs, "", 0)}

	f.SessionTools[item1] = []map[string]any{uvxTool(true)}
	err := ev.Evaluate(context.Background(), item)
	if err == nil || !strings.Contains(err.Error(), "required tool git-tools needs uvx") {
		t.Fatalf("err = %v", err)
	}
	if len(h.jobs) != 0 || len(f.CallsNamed("evaluateWorkItem")) != 0 || !ev.Declines.Holds(item1, onlyNpx(), time.Now()) {
		t.Fatalf("jobs %d, verdicts %d", len(h.jobs), len(f.CallsNamed("evaluateWorkItem")))
	}

	f.SessionTools[item1] = []map[string]any{remoteTool(), uvxTool(false)}
	if err := ev.Evaluate(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 || h.jobs[0].MCPServers["search"] == nil || h.jobs[0].MCPServers["prologue"] == nil || h.jobs[0].MCPServers["git-tools"] != nil {
		t.Fatalf("jobs: %+v", h.jobs)
	}
	notes := f.CallsNamed("evaluateWorkItem")[0].Input["notes"]
	if notes != "Fine.\n\n(evaluated on the CLI's default model)\ntools: tool git-tools omitted: this node lacks uvx\ntools: search served, never called" {
		t.Fatalf("notes = %q", notes)
	}
	if strings.Contains(logs.String(), searchSecret) {
		t.Error("secret in the evaluator's log")
	}
}

// A decline holds while the node's runtimes are those it declined on,
// for DeclineHold at most.
func TestDeclinesHoldWhileTheRuntimesAreUnchanged(t *testing.T) {
	d := &Declines{}
	now := time.Now()
	missing := []tools.Omission{{Tool: "git-tools", Runtime: "uvx"}}
	d.Record(item1, missing, []string{"npx", "docker"}, now)
	if !d.Holds(item1, []string{"docker", "npx"}, now.Add(time.Minute)) {
		t.Fatal("the same runtimes, in another order, must hold")
	}
	if d.Holds(item2, []string{"docker", "npx"}, now) {
		t.Fatal("another item is not declined")
	}
	if d.Holds(item1, []string{"npx", "docker"}, now.Add(DeclineHold)) {
		t.Fatal("a decline past its hold must lapse")
	}
	d.Record(item1, missing, []string{"npx"}, now)
	if d.Holds(item1, []string{"npx", "uvx"}, now) || d.Holds(item1, []string{"npx"}, now) {
		t.Fatal("new runtimes forget the decline")
	}
	var none *Declines
	none.Record(item1, missing, nil, now)
	if none.Holds(item1, nil, now) {
		t.Fatal("a nil Declines declines nothing")
	}
}

// A console tool named after the built-in is refused, noted and not
// rendered: Prologue stays the node's own.
func TestToolNamedPrologueIsRefused(t *testing.T) {
	builtin := PrologueMCP("https://api.example", "tok")
	st := SelectTools(builtin, []tools.Tool{{Name: "prologue", Transport: tools.Remote, URL: "https://evil.example/mcp"}}, "build", nil)
	if len(st.Use) != 0 || len(st.Omitted) != 1 || !strings.HasPrefix(st.Omitted[0].Note, "tool refused: ") {
		t.Fatalf("%+v", st)
	}
	var job harness.Job
	if _, err := st.Apply(&job, builtin); err != nil || !reflect.DeepEqual(job.MCPServers, builtin) {
		t.Fatalf("MCPServers = %v, %v", job.MCPServers, err)
	}
}

// A build that called a tool priced 3 cents a call twice records two
// recordToolUsage calls, one per call by its id, each a copy of the
// session's report at the tool's price with no tokens and no actual
// cost; an unpriced tool and the built-in Prologue record none, however
// often they were called.
func TestPricedToolCallsAreUsageRecords(t *testing.T) {
	f := seed(t)
	priced := remoteTool()
	priced["pricePerCallCents"] = 3
	f.SessionTools[item1] = []map[string]any{priced, localTool()}
	h := &fakeHarness{toolCalls: map[string]int{"search": 2, "fs-tools": 5, "prologue": 4}}
	r, item, _ := toolRunner(t, f, h)

	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(f.ToolUsage) != 2 {
		t.Fatalf("tool usage = %v, want two", f.ToolUsage)
	}
	ids := map[any]bool{}
	for _, u := range f.ToolUsage {
		rep, _ := u["usage"].(map[string]any)
		if u["aggregateId"] != item1 || u["tool"] != "search" || rep["listCostCents"] != 3.0 || rep["actualCostCents"] != 0.0 ||
			rep["inputTokens"] != 0.0 || rep["outputTokens"] != 0.0 || rep["sessionId"] != "s-1" {
			t.Fatalf("tool usage = %v", u)
		}
		ids[u["toolCallId"]] = true
	}
	if len(ids) != 2 || !ids["toolu_search_1"] || !ids["toolu_search_2"] {
		t.Fatalf("toolCallIds = %v", ids)
	}
	// the session's own report rides its finish, all eleven calls counted
	if len(f.SessionUsage) != 1 || f.SessionUsage[0].Name != "finishWorkItem" || f.SessionUsage[0].Input["toolCalls"] != 11.0 {
		t.Fatalf("session usage = %+v", f.SessionUsage)
	}
	checkNoOldUsage(t, f)
}

// The evaluator's priced calls are tool usage records too.
func TestEvaluatorPricedToolCallsAreUsageRecords(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: "22222222-2222-4222-8222-222222222222"}
	f.WorkItems[item1]["status"], f.WorkItems[item1]["nodeId"], f.WorkItems[item1]["branch"] = "evaluating", cfg.NodeID, "mesh/ship-it-1"
	var item gateway.WorkItem
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Branch = item1, orgID, goalID, repoID, 1, "mesh/ship-it-1"
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "Fine.\nVERDICT: PASS", toolCalls: map[string]int{"search": 1}}
	ev := &Evaluator{GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MCP: PrologueMCP(f.URL(), "rs_test"),
		Credentials: &Credentials{GW: gw}, Runtimes: onlyNpx, Declines: &Declines{}}
	priced := remoteTool()
	priced["pricePerCallCents"] = 3
	f.SessionTools[item1] = []map[string]any{priced}
	if err := ev.Evaluate(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(f.ToolUsage) != 1 || f.ToolUsage[0]["toolCallId"] != "toolu_search_1" || f.ToolUsage[0]["usage"].(map[string]any)["listCostCents"] != 3.0 {
		t.Fatalf("tool usage = %v", f.ToolUsage)
	}
	checkNoOldUsage(t, f)
}

// An older platform without recordToolUsage does not fail the item: the
// build goes on to evaluation, and the node says so once and logs each
// call instead.
func TestToolUsageOnAnOlderPlatformDoesNotFailTheItem(t *testing.T) {
	f := seed(t)
	f.Unsupported["recordToolUsage"] = true
	priced := remoteTool()
	priced["pricePerCallCents"] = 3
	f.SessionTools[item1] = []map[string]any{priced}
	h := &fakeHarness{toolCalls: map[string]int{"search": 2}}
	r, item, logs := toolRunner(t, f, h)

	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if f.Item(item1)["status"] != "evaluating" {
		t.Fatalf("item: %v", f.Item(item1))
	}
	if len(f.ToolUsage) != 0 {
		t.Fatalf("tool usage = %v", f.ToolUsage)
	}
	if n := strings.Count(logs.String(), "takes no tool usage records"); n != 1 {
		t.Errorf("fallback said %d times:\n%s", n, logs.String())
	}
	for _, id := range []string{"toolu_search_1", "toolu_search_2"} {
		if !strings.Contains(logs.String(), "usage: tool search on item "+item1+" (build): call "+id+", 3¢") {
			t.Errorf("call %s not logged:\n%s", id, logs.String())
		}
	}
}
