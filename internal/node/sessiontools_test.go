package node

import (
	"reflect"
	"strings"
	"testing"
	"time"

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

func slicesContain(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
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
// rendered: runsheet stays the node's own. A tool named as an old
// name (mesh-context, tools.BuiltinAlias) no longer clashes and is rendered like any other.
func TestToolNamedMeshContextIsRefused(t *testing.T) {
	builtin := MeshContextHTTP("https://api.example", "tok")
	st := SelectTools(builtin, []tools.Tool{{Name: "runsheet", Transport: tools.Remote, URL: "https://evil.example/mcp"}}, "build", nil)
	if len(st.Use) != 0 || len(st.Omitted) != 1 || !strings.HasPrefix(st.Omitted[0].Note, "tool refused: ") {
		t.Fatalf("%+v", st)
	}
	var job harness.Job
	if _, err := st.Apply(&job, builtin); err != nil || !reflect.DeepEqual(job.MCPServers, builtin) {
		t.Fatalf("MCPServers = %v, %v", job.MCPServers, err)
	}

	// the alias, the built-in's old name, is read as an alias until
	// 2026-10-28 (tools.IsBuiltin) but never clashes
	st = SelectTools(builtin, []tools.Tool{{Name: tools.BuiltinAlias(), Transport: tools.Remote, URL: "https://ok.example/mcp"}}, "build", nil)
	if len(st.Use) != 1 || len(st.Omitted) != 0 {
		t.Fatalf("%s: %+v", tools.BuiltinAlias(), st)
	}
	job = harness.Job{}
	if _, err := st.Apply(&job, builtin); err != nil || job.MCPServers[tools.BuiltinAlias()] == nil {
		t.Fatalf("MCPServers = %v, %v", job.MCPServers, err)
	}
}

const dockerSecret = "key-docker-9a1f02"

// dockerTool is a required local tool on docker; brave and perplexity
// (braveTool, perplexityTool) are the search tools a project attaches
// for the planner and director roles.
func dockerTool() map[string]any {
	return map[string]any{"id": "t-dock", "slug": "crawler", "name": "Crawler", "kind": "local_mcp", "need": "required", "runtime": "docker", "command": "example/crawler",
		"account": map[string]any{"id": "a-dock", "label": "crawler"},
		"headers": []any{}, "env": []any{map[string]any{"name": "CRAWLER_KEY", "value": dockerSecret}}}
}

// checkToolUsageOn is the fake's recordToolUsage inputs: one per call
// id of the priced search tool, each on aggregate.
func checkToolUsageOn(t *testing.T, f *gatewaytest.Fake, aggregate string) {
	t.Helper()
	if len(f.ToolUsage) != 2 {
		t.Fatalf("tool usage = %v, want two", f.ToolUsage)
	}
	ids := map[any]bool{}
	for _, u := range f.ToolUsage {
		rep, _ := u["usage"].(map[string]any)
		if u["aggregateId"] != aggregate || u["tool"] != "search" || rep["listCostCents"] != 3.0 || rep["actualCostCents"] != 0.0 ||
			rep["inputTokens"] != 0.0 || rep["sessionId"] != "s-1" {
			t.Fatalf("tool usage = %v", u)
		}
		ids[u["toolCallId"]] = true
	}
	if len(ids) != 2 || !ids["toolu_search_1"] || !ids["toolu_search_2"] {
		t.Fatalf("toolCallIds = %v", ids)
	}
}
