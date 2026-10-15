// An external test: harness imports tools, and these tests check what
// harness.MCPArgs makes of the rendering.
package tools_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"projectmesh.io/mesh-cli/internal/harness"
	. "projectmesh.io/mesh-cli/internal/tools"
)

const (
	bearerSecret = "tok-remote-8f2c1a"
	headerSecret = "key-header-77b3e0"
	envSecret    = "key-local-3d9a42"
)

func meshContext() map[string]any {
	return map[string]any{"runsheet": map[string]any{
		"type": "http", "url": "https://api.runsheet.dev/mcp",
		"headers": map[string]string{"Authorization": "Bearer ${MESH_CONTEXT_TOKEN}"},
	}}
}

func sample() []Tool {
	return []Tool{
		{
			Name: "search", Transport: Remote, URL: "https://search.example/mcp",
			Headers: map[string]string{"X-Region": "au"},
			Secrets: map[string]string{"Authorization": "Bearer " + bearerSecret, "X-Api-Key": headerSecret},
		},
		{
			Name: "fs-tools", Transport: Local, Runtime: Npx, Command: "@example/fs-mcp", Args: []string{"--root", "."},
			Env:      map[string]string{"LOG_LEVEL": "warn"},
			Secrets:  map[string]string{"FS_API_KEY": envSecret},
			Required: true,
		},
	}
}

func TestClaudeCodeRendersRemoteLocalAndBuiltin(t *testing.T) {
	servers, env, err := ClaudeCode(meshContext(), sample())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"runsheet", "search", "fs-tools"} {
		if _, ok := servers[n]; !ok {
			t.Fatalf("servers lack %s: %v", n, servers)
		}
	}
	remote := servers["search"].(map[string]any)
	if remote["type"] != "http" || remote["url"] != "https://search.example/mcp" {
		t.Fatalf("remote = %v", remote)
	}
	wantHeaders := map[string]string{
		"X-Region":      "au",
		"Authorization": "Bearer ${MESH_TOOL_SEARCH_AUTHORIZATION}",
		"X-Api-Key":     "${MESH_TOOL_SEARCH_X_API_KEY}",
	}
	if !reflect.DeepEqual(remote["headers"], wantHeaders) {
		t.Fatalf("headers = %v", remote["headers"])
	}
	local := servers["fs-tools"].(map[string]any)
	if local["command"] != "npx" || !reflect.DeepEqual(local["args"], []string{"-y", "@example/fs-mcp", "--root", "."}) {
		t.Fatalf("local = %v", local)
	}
	if !reflect.DeepEqual(local["env"], map[string]string{"LOG_LEVEL": "warn", "FS_API_KEY": "${MESH_TOOL_FS_TOOLS_FS_API_KEY}"}) {
		t.Fatalf("local env = %v", local["env"])
	}
	wantEnv := []string{
		"MESH_TOOL_SEARCH_AUTHORIZATION=" + bearerSecret,
		"MESH_TOOL_SEARCH_X_API_KEY=" + headerSecret,
		"MESH_TOOL_FS_TOOLS_FS_API_KEY=" + envSecret,
	}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("env = %v", env)
	}

	args := harness.MCPArgs(servers)
	if len(args) != 4 || args[2] != "--allowedTools" || args[3] != "mcp__fs-tools__*,mcp__runsheet__*,mcp__search__*" {
		t.Fatalf("MCPArgs = %v", args)
	}
}

func TestLocalRuntimes(t *testing.T) {
	for _, c := range []struct {
		tool     Tool
		cmd      string
		wantArgs []string
	}{
		{Tool{Name: "a", Transport: Local, Runtime: Uvx, Command: "mcp-server-git", Args: []string{"--repo", "."}},
			"uvx", []string{"mcp-server-git", "--repo", "."}},
		{Tool{Name: "b", Transport: Local, Runtime: Docker, Command: "ghcr.io/example/mcp:1", Args: []string{"serve"},
			Env: map[string]string{"MODE": "ro"}, Secrets: map[string]string{"TOKEN": "t"}},
			"docker", []string{"run", "-i", "--rm", "-e", "MODE", "-e", "TOKEN", "ghcr.io/example/mcp:1", "serve"}},
	} {
		servers, _, err := ClaudeCode(nil, []Tool{c.tool})
		if err != nil {
			t.Fatal(err)
		}
		s := servers[c.tool.Name].(map[string]any)
		if s["command"] != c.cmd || !reflect.DeepEqual(s["args"], c.wantArgs) {
			t.Fatalf("%s: %v %v", c.tool.Runtime, s["command"], s["args"])
		}
	}
}

func TestClaudeCodeRefusesClashWithBuiltin(t *testing.T) {
	_, _, err := ClaudeCode(meshContext(), []Tool{{Name: "runsheet", Transport: Remote, URL: "https://x"}})
	if err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("err = %v", err)
	}
	if _, err := WriteCodexConfig(t.TempDir(), []Tool{{Name: "runsheet", Transport: Remote, URL: "https://x"}}); err == nil {
		t.Fatal("codex: clash with runsheet accepted")
	}
}

// mesh-context, the built-in's name until 2026-10-15, is an alias read
// on adoption (IsBuiltin) and never the clash: a console tool named that
// is an ordinary tool, as runsheet is refused.
func TestToolNamedMeshContextIsNotAClash(t *testing.T) {
	servers, _, err := ClaudeCode(meshContext(), []Tool{{Name: "mesh-context", Transport: Remote, URL: "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := servers["mesh-context"]; !ok {
		t.Fatalf("servers = %v", servers)
	}
	if _, err := WriteCodexConfig(t.TempDir(), []Tool{{Name: "mesh-context", Transport: Remote, URL: "https://x"}}); err != nil {
		t.Fatalf("codex: %v", err)
	}
}

// BuiltinAlias is the built-in's old name, a read-side alias until
// 2026-10-28 (IsBuiltin) and never the clash: a console tool named that
// is an ordinary tool.
func TestToolNamedAsTheAliasIsRendered(t *testing.T) {
	servers, _, err := ClaudeCode(meshContext(), []Tool{{Name: BuiltinAlias(), Transport: Remote, URL: "https://x"}})
	if err != nil {
		t.Fatalf("claude: %s refused: %v", BuiltinAlias(), err)
	}
	if _, ok := servers[BuiltinAlias()]; !ok {
		t.Fatalf("servers lack %s: %v", BuiltinAlias(), servers)
	}
	if _, err := WriteCodexConfig(t.TempDir(), []Tool{{Name: BuiltinAlias(), Transport: Remote, URL: "https://x"}}); err != nil {
		t.Fatalf("codex: %s refused: %v", BuiltinAlias(), err)
	}
}

type codexConfig struct {
	MCPServers map[string]struct {
		URL               string            `toml:"url"`
		BearerTokenEnvVar string            `toml:"bearer_token_env_var"`
		HTTPHeaders       map[string]string `toml:"http_headers"`
		EnvHTTPHeaders    map[string]string `toml:"env_http_headers"`
		Command           string            `toml:"command"`
		Args              []string          `toml:"args"`
		Env               map[string]string `toml:"env"`
		EnvVars           []string          `toml:"env_vars"`
		ApprovalMode      string            `toml:"default_tools_approval_mode"`
	} `toml:"mcp_servers"`
}

func TestWriteCodexConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codex")
	env, err := WriteCodexConfig(home, sample())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	var cfg codexConfig
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if len(cfg.MCPServers) != 2 {
		t.Fatalf("servers = %v", cfg.MCPServers)
	}
	r := cfg.MCPServers["search"]
	if r.URL != "https://search.example/mcp" || r.BearerTokenEnvVar != "MESH_TOOL_SEARCH_AUTHORIZATION" ||
		!reflect.DeepEqual(r.HTTPHeaders, map[string]string{"X-Region": "au"}) ||
		!reflect.DeepEqual(r.EnvHTTPHeaders, map[string]string{"X-Api-Key": "MESH_TOOL_SEARCH_X_API_KEY"}) {
		t.Fatalf("remote = %+v", r)
	}
	for name, srv := range cfg.MCPServers {
		// codex exec refuses an unapproved MCP call under workspace-write
		if srv.ApprovalMode != "approve" {
			t.Errorf("%s: default_tools_approval_mode %q", name, srv.ApprovalMode)
		}
	}
	l := cfg.MCPServers["fs-tools"]
	if l.Command != "npx" || !reflect.DeepEqual(l.Args, []string{"-y", "@example/fs-mcp", "--root", "."}) ||
		!reflect.DeepEqual(l.Env, map[string]string{"LOG_LEVEL": "warn"}) || !reflect.DeepEqual(l.EnvVars, []string{"FS_API_KEY"}) {
		t.Fatalf("local = %+v", l)
	}
	wantEnv := []string{
		"MESH_TOOL_SEARCH_AUTHORIZATION=" + bearerSecret,
		"MESH_TOOL_SEARCH_X_API_KEY=" + headerSecret,
		"FS_API_KEY=" + envSecret,
	}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("env = %v", env)
	}
}

func TestCodexRefusesOneSecretKeyWithTwoValues(t *testing.T) {
	a := Tool{Name: "a", Transport: Local, Runtime: Npx, Command: "a", Secrets: map[string]string{"K": "1"}}
	b := Tool{Name: "b", Transport: Local, Runtime: Npx, Command: "b", Secrets: map[string]string{"K": "2"}}
	if _, err := WriteCodexConfig(t.TempDir(), []Tool{a, b}); err == nil {
		t.Fatal("clashing secret env accepted")
	}
}

func TestSecretsOnlyInEnv(t *testing.T) {
	secrets := []string{bearerSecret, headerSecret, envSecret}
	servers, claudeEnv, err := ClaudeCode(meshContext(), sample())
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(servers)
	argv := strings.Join(harness.MCPArgs(servers), " ")
	home := t.TempDir()
	codexEnv, err := WriteCodexConfig(home, sample())
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	for _, s := range secrets {
		for where, text := range map[string]string{"servers JSON": string(js), "MCPArgs": argv, "config.toml": string(cfg)} {
			if strings.Contains(text, s) {
				t.Errorf("secret %s in %s", s, where)
			}
		}
		for which, env := range map[string][]string{"claude": claudeEnv, "codex": codexEnv} {
			if !strings.Contains(strings.Join(env, "\n"), s) {
				t.Errorf("secret %s not in %s env", s, which)
			}
		}
	}
}

func TestSelect(t *testing.T) {
	tools := []Tool{
		{Name: "web", Transport: Remote, URL: "https://w"},
		{Name: "git", Transport: Local, Runtime: Uvx, Command: "mcp-server-git"},
		{Name: "browser", Transport: Local, Runtime: Docker, Command: "img", Required: false},
		{Name: "db", Transport: Local, Runtime: Docker, Command: "img", Required: true},
		{Name: "deploy", Transport: Remote, URL: "https://d", Roles: []string{"build"}},
	}
	use, omitted, missing := Select(tools, "evaluate", []string{Npx, Uvx})
	var names []string
	for _, u := range use {
		names = append(names, u.Name)
	}
	if !reflect.DeepEqual(names, []string{"web", "git"}) {
		t.Fatalf("use = %v", names)
	}
	if len(omitted) != 1 || omitted[0].Tool != "browser" || omitted[0].Runtime != "docker" ||
		omitted[0].Note != "tool browser omitted: this node lacks docker" {
		t.Fatalf("omitted = %+v", omitted)
	}
	if len(missing) != 1 || missing[0].Tool != "db" || missing[0].Runtime != "docker" ||
		missing[0].Note != "required tool db needs docker, which this node lacks" {
		t.Fatalf("missing = %+v", missing)
	}
	if use, _, _ := Select(tools, "build", []string{Npx, Uvx, Docker}); len(use) != 5 {
		t.Fatalf("build with every runtime = %d tools", len(use))
	}
}

// Each role in Roles is served to its own session kind alone: a tool
// attached for one role never reaches the other four kinds.
func TestEachRoleIsServedToItsKindAlone(t *testing.T) {
	kinds := []string{"build", "evaluate", "plan", "research", "rebase"}
	for _, role := range Roles {
		tool := Tool{Name: role + "-tool", Transport: Remote, URL: "https://x", Roles: []string{role}}
		for _, kind := range kinds {
			use, _, _ := Select([]Tool{tool}, RoleFor(kind), nil)
			got := len(use) == 1
			want := RoleFor(kind) == role
			if got != want {
				t.Errorf("role %s, kind %s (RoleFor=%s): served = %v, want %v", role, kind, RoleFor(kind), got, want)
			}
		}
	}
}

// The old kind names mesh-specs trd §3 documented still serve, whether
// spelled on the tool's roles or passed as the session's role.
func TestOldRoleSpellingsStillServe(t *testing.T) {
	if got := RoleFor("build"); got != "builder" {
		t.Errorf(`RoleFor("build") = %q, want "builder"`, got)
	}
	if got := RoleFor("evaluate"); got != "evaluator" {
		t.Errorf(`RoleFor("evaluate") = %q, want "evaluator"`, got)
	}
	if got := RoleFor("plan"); got != "planner" {
		t.Errorf(`RoleFor("plan") = %q, want "planner"`, got)
	}
	if got := RoleFor("research"); got != "director" {
		t.Errorf(`RoleFor("research") = %q, want "director"`, got)
	}
	if got := RoleFor("rebase"); got != "rebase" {
		t.Errorf(`RoleFor("rebase") = %q, want "rebase"`, got)
	}
	if got := RoleFor("mystery"); got != "mystery" {
		t.Errorf(`RoleFor("mystery") = %q, want "mystery" (unknown kind returns itself)`, got)
	}

	// an old-spelled role on the tool serves the new-named role passed in
	oldSpelled := Tool{Name: "old", Transport: Remote, URL: "https://x", Roles: []string{"research"}}
	if use, _, _ := Select([]Tool{oldSpelled}, "director", nil); len(use) != 1 {
		t.Fatalf("tool with roles [research] not served to role director: %v", use)
	}
	// a new-named role on the tool serves the old-spelled role passed in
	newSpelled := Tool{Name: "new", Transport: Remote, URL: "https://x", Roles: []string{"director"}}
	if use, _, _ := Select([]Tool{newSpelled}, "research", nil); len(use) != 1 {
		t.Fatalf("tool with roles [director] not served to role research: %v", use)
	}
}

func TestProbe(t *testing.T) {
	onPath := map[string]bool{"npx": true, "docker": true, "claude": true}
	look := func(n string) (string, error) {
		if onPath[n] {
			return "/usr/bin/" + n, nil
		}
		return "", errors.New("not found")
	}
	if got := ProbeRuntimes(look); !reflect.DeepEqual(got, []string{"npx", "docker"}) {
		t.Fatalf("runtimes = %v", got)
	}
	if got := ProbeAdapters(look); !reflect.DeepEqual(got, []string{"claude-code"}) {
		t.Fatalf("adapters = %v", got)
	}
}

func TestRedact(t *testing.T) {
	_, env, err := ClaudeCode(nil, sample())
	if err != nil {
		t.Fatal(err)
	}
	got := Redact("mcp search failed: 401 for "+bearerSecret+" (key "+headerSecret+")", env)
	if got != "mcp search failed: 401 for [redacted] (key [redacted])" {
		t.Fatalf("got %q", got)
	}
	if got := Redact("abcdef and abc", []string{"A=abc", "B=abcdef", "C="}); got != "[redacted] and [redacted]" {
		t.Fatalf("longest first: %q", got)
	}
}

// IsBuiltin knows the built-in by its name, runsheet, and its old ones,
// mesh-context and BuiltinAlias, read as aliases.
func TestIsBuiltin(t *testing.T) {
	if Builtin != "runsheet" {
		t.Fatalf("Builtin = %q", Builtin)
	}
	if a := BuiltinAliases(); len(a) != 2 || a[0] != "mesh-context" || a[1] != BuiltinAlias() {
		t.Fatalf("BuiltinAliases() = %q", a)
	}
	for name, want := range map[string]bool{"runsheet": true, "mesh-context": true, BuiltinAlias(): true, "search": false, "": false} {
		if got := IsBuiltin(name); got != want {
			t.Errorf("IsBuiltin(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestToolJSONMatchesContract(t *testing.T) {
	var tool Tool
	in := `{"name":"n","transport":"local","runtime":"npx","command":"c","args":["a"],"env":{"E":"1"},"required":true,"roles":["build"],"pricePerCallCents":3}`
	if err := json.Unmarshal([]byte(in), &tool); err != nil {
		t.Fatal(err)
	}
	if tool.Name != "n" || tool.Runtime != Npx || !tool.Required || tool.PricePerCallCents != 3 || tool.Roles[0] != "build" {
		t.Fatalf("%+v", tool)
	}
}

// The Codex preflight checks the server this package reserves.
func TestBuiltinIsTheHarnessPreflights(t *testing.T) {
	if Builtin != harness.BuiltinMCP {
		t.Fatalf("tools.Builtin %q, harness.BuiltinMCP %q", Builtin, harness.BuiltinMCP)
	}
}
