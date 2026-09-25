package tools

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
)

const (
	bearerSecret = "tok-remote-8f2c1a"
	headerSecret = "key-header-77b3e0"
	envSecret    = "key-local-3d9a42"
)

func prologue() map[string]any {
	return map[string]any{"prologue": map[string]any{
		"type": "http", "url": "https://api.runsheet.dev/mcp",
		"headers": map[string]string{"Authorization": "Bearer ${MESH_PROLOGUE_TOKEN}"},
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
	servers, env, err := ClaudeCode(prologue(), sample())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"prologue", "search", "fs-tools"} {
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
	if len(args) != 4 || args[2] != "--allowedTools" || args[3] != "mcp__fs-tools__*,mcp__prologue__*,mcp__search__*" {
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
	_, _, err := ClaudeCode(prologue(), []Tool{{Name: "prologue", Transport: Remote, URL: "https://x"}})
	if err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("err = %v", err)
	}
	if _, err := WriteCodexConfig(t.TempDir(), []Tool{{Name: "prologue", Transport: Remote, URL: "https://x"}}); err == nil {
		t.Fatal("codex: clash with prologue accepted")
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
	servers, claudeEnv, err := ClaudeCode(prologue(), sample())
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
	if got := ProbeAdapters(look); !reflect.DeepEqual(got, []string{"claude"}) {
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
