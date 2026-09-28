package gateway_test

import (
	"context"
	"reflect"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/tools"
)

// account is a served tool's account, as itemTools names it.
func account(id, label string) map[string]any { return map[string]any{"id": id, "label": label} }

// The claim-time read of an item carries its tools — itemTools(itemId)
// beside itemCredentials — remote and local, in runsheet's shape (ADR
// 020), each becoming the node's Tool: named by its slug, a remote_mcp
// remote and a local_mcp local, `required` from its need. A setting of a
// tool served through an account is a secret (it may be that account's);
// a tool served with none has plain headers and env; one sourced from
// the item's GitHub token reads the session's GH_TOKEN. The platform's
// built-in (mesh-context) and a builtin switch are not tools of the
// read: the node attaches its own.
func TestSessionCarriesTools(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.SessionCredentials[itemID] = []map[string]any{anthropicSubscription("c1", "max-a", "sk-ant-oat01-x")}
	f.SessionTools[itemID] = []map[string]any{
		{"id": "t0", "slug": "mesh-context", "name": "Mesh context", "kind": "remote_mcp", "need": "required", "url": "https://api.runsheet.dev/mesh/v1", "builtIn": true, "account": nil,
			"headers": []any{map[string]any{"name": "Authorization", "value": "Bearer rs_x"}}, "env": []any{}},
		{"id": "t1", "slug": "linear", "name": "Linear", "kind": "remote_mcp", "need": "required", "url": "https://mcp.linear.app/mcp", "builtIn": false, "account": account("a1", "linear-eng"),
			"headers": []any{map[string]any{"name": "X-Team", "value": "eng"}, map[string]any{"name": "Authorization", "value": "Bearer lin_x"}}, "env": []any{}},
		{"id": "t2", "slug": "pg", "name": "Postgres", "kind": "local_mcp", "need": "nice_to_have", "runtime": "npx", "command": "@modelcontextprotocol/server-postgres", "args": []any{"--ro"}, "builtIn": false, "account": account("a2", "pg-ro"),
			"headers": []any{}, "env": []any{map[string]any{"name": "PGHOST", "value": "db"}, map[string]any{"name": "PGPASSWORD", "value": "pw"}}},
		{"id": "t3", "slug": "github", "name": "GitHub", "kind": "local_mcp", "need": "required", "runtime": "docker", "command": "ghcr.io/github/github-mcp-server", "args": []any{}, "builtIn": false, "account": nil,
			"headers": []any{}, "env": []any{map[string]any{"name": "GITHUB_PERSONAL_ACCESS_TOKEN", "source": "item_github_token"}, map[string]any{"name": "GITHUB_TOOLSETS", "value": "repos,pull_requests"}}},
		{"id": "t4", "slug": "web-search", "name": "Web search", "kind": "builtin", "need": "nice_to_have", "builtin": "web-search", "builtIn": false, "account": nil, "headers": []any{}, "env": []any{}},
		{"id": "t5", "slug": "Bad Name", "name": "Bad", "kind": "remote_mcp", "need": "nice_to_have", "url": "https://x", "builtIn": false, "account": nil, "headers": []any{}, "env": []any{}},
	}
	gw := gateway.New(f.URL(), "rs_test")
	got, err := gw.Session(context.Background(), gateway.SessionRef{Kind: "build", ItemID: itemID}, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	want := []tools.Tool{
		{Name: "linear", Transport: tools.Remote, URL: "https://mcp.linear.app/mcp", Required: true,
			Secrets: map[string]string{"X-Team": "eng", "Authorization": "Bearer lin_x"}},
		{Name: "pg", Transport: tools.Local, Runtime: tools.Npx, Command: "@modelcontextprotocol/server-postgres", Args: []string{"--ro"},
			Secrets: map[string]string{"PGHOST": "db", "PGPASSWORD": "pw"}},
		{Name: "github", Transport: tools.Local, Runtime: tools.Docker, Command: "ghcr.io/github/github-mcp-server", Required: true,
			Env: map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": gateway.ItemGitHubTokenVar, "GITHUB_TOOLSETS": "repos,pull_requests"}},
	}
	if !reflect.DeepEqual(got.Tools, want) {
		t.Fatalf("tools = %+v\nwant %+v", got.Tools, want)
	}
	if len(got.Credentials) != 1 || got.Credentials[0].Label != "max-a" {
		t.Fatalf("credentials = %v", got.Credentials)
	}
	if reads := f.CallsNamed("itemTools"); len(reads) != 1 || reads[0].Input["itemId"] != itemID {
		t.Fatalf("itemTools calls = %v", reads)
	}
	// the rendering takes what the read gave: the secrets in env, the
	// token's variable left for the session to expand
	servers, env, err := tools.ClaudeCode(map[string]any{"mesh-context": map[string]any{"type": "http", "url": "https://p"}}, got.Tools)
	if err != nil {
		t.Fatal(err)
	}
	gh, _ := servers["github"].(map[string]any)
	if vars, _ := gh["env"].(map[string]string); vars["GITHUB_PERSONAL_ACCESS_TOKEN"] != gateway.ItemGitHubTokenVar {
		t.Errorf("github env = %v", gh["env"])
	}
	if len(env) != 4 {
		t.Errorf("env = %v, want linear's two and pg's two", env)
	}
}

// A builtIn row (the platform's own mesh-context) beside a remote tool
// yields only the remote tool: the built-in is the node's own attachment
// and is never rendered twice.
func TestSessionExcludesTheBuiltinRow(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.SessionCredentials[itemID] = []map[string]any{anthropicSubscription("c1", "max-a", "s")}
	f.SessionTools[itemID] = []map[string]any{
		{"id": "t0", "slug": "mesh-context", "name": "Mesh context", "kind": "remote_mcp", "need": "required", "url": "https://api.runsheet.dev/mesh/v1", "builtIn": true, "account": nil,
			"headers": []any{}, "env": []any{}},
		{"id": "t1", "slug": "linear", "name": "Linear", "kind": "remote_mcp", "need": "required", "url": "https://mcp.linear.app/mcp", "builtIn": false, "account": nil,
			"headers": []any{}, "env": []any{}},
	}
	gw := gateway.New(f.URL(), "rs_test")
	got, err := gw.Session(context.Background(), gateway.SessionRef{Kind: "build", ItemID: itemID}, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	want := []tools.Tool{{Name: "linear", Transport: tools.Remote, URL: "https://mcp.linear.app/mcp", Required: true}}
	if !reflect.DeepEqual(got.Tools, want) {
		t.Fatalf("tools = %+v\nwant %+v", got.Tools, want)
	}
}

// A platform whose read carries no tools — none configured — decodes to
// an empty list; one older than tools-in-the-console refuses the field,
// and the client asks again for the credentials alone, and from then on
// asks that session kind for credentials alone.
func TestSessionWithoutTools(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.SessionCredentials[itemID] = []map[string]any{anthropicSubscription("c1", "max-a", "s")}
	gw := gateway.New(f.URL(), "rs_test")
	ref := gateway.SessionRef{Kind: "build", ItemID: itemID}
	got, err := gw.Session(context.Background(), ref, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 0 || len(got.Credentials) != 1 {
		t.Fatalf("session = %+v, want its credential and no tools", got)
	}

	old := gatewaytest.New("rs_test")
	defer old.Close()
	old.UnknownFields["itemTools"] = true
	old.SessionCredentials[itemID] = f.SessionCredentials[itemID]
	old.SessionTools[itemID] = []map[string]any{{"id": "t", "slug": "x", "kind": "remote_mcp", "url": "https://x"}}
	gw = gateway.New(old.URL(), "rs_test")
	for i := 0; i < 2; i++ {
		got, err := gw.Session(context.Background(), ref, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Tools != nil || len(got.Credentials) != 1 {
			t.Fatalf("read %d: session = %+v, want its credential and nil tools", i, got)
		}
	}
	// two reads with tools refused once, then one without: three
	// credential reads, and no second try at the tools
	if n := len(old.CallsNamed("itemCredentials")); n != 2 {
		t.Fatalf("itemCredentials reads = %d, want 2 (the refused one is not recorded)", n)
	}
	creds, err := gw.SessionCredentials(context.Background(), ref, nodeID)
	if err != nil || len(creds) != 1 {
		t.Fatalf("SessionCredentials = %v, %v", creds, err)
	}
}

// The heartbeat carries runtimes and adapters when set, and neither key
// when not, so a platform without the fields still takes it.
func TestHeartbeatRuntimesAdapters(t *testing.T) {
	f, gw := registeredFake(t)
	ctx := context.Background()
	if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, gateway.HeartbeatExtras{Runtimes: []string{"npx", "docker"}, Adapters: []string{"claude-code"}}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	if got := hb[0].Input["runtimes"]; !reflect.DeepEqual(got, []any{"npx", "docker"}) {
		t.Fatalf("runtimes = %#v", got)
	}
	if got := hb[0].Input["adapters"]; !reflect.DeepEqual(got, []any{"claude-code"}) {
		t.Fatalf("adapters = %#v", got)
	}
	for _, x := range [][]gateway.HeartbeatExtras{nil, {{}}, {{Draining: true, Runtimes: []string{}}}} {
		if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 0, x...); err != nil {
			t.Fatal(err)
		}
	}
	for i, hb := range f.CallsNamed("heartbeatNode")[1:] {
		for _, k := range []string{"runtimes", "adapters"} {
			if v, ok := hb.Input[k]; ok {
				t.Fatalf("heartbeat %d sent %s = %#v", i+1, k, v)
			}
		}
	}
}
