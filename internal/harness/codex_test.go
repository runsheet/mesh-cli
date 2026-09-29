package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// testServers are a job's MCP servers: mesh-context over http with the
// node's token, and a local stdio server with a secret in its env.
func testServers() map[string]any {
	return map[string]any{
		"mesh-context": map[string]any{"type": "http", "url": "https://api.test/mcp",
			"headers": map[string]string{"Authorization": "Bearer rs_secret"}},
		"local": map[string]any{"command": "mcp-local", "args": []any{"--root", "."}, "env": map[string]any{"LOCAL_TOKEN": "local-secret"}},
	}
}

// The argv for a job with a model and MCP servers: exec in JSON, the
// workspace-write sandbox with the network on (its tests bind
// loopback), the model and `-` for the prompt on stdin.
// Neither the prompt nor the servers are on it: those go in the
// session's CODEX_HOME/config.toml.
func TestCodexArgs(t *testing.T) {
	c := &Codex{ExtraArgs: []string{"--skip-git-repo-check"}}
	job := Job{Prompt: "build the thing", Model: "gpt-6-sol", MCPServers: testServers()}
	want := []string{"exec", "--json", "--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true", "--model", "gpt-6-sol", "--skip-git-repo-check", "-"}
	got := c.Args(job)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args\n got %q\nwant %q", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, "build the thing") || strings.Contains(a, "secret") {
			t.Fatalf("argv carries the prompt or a secret: %q", a)
		}
	}
	if got := c.Args(Job{Prompt: "hi"}); !reflect.DeepEqual(got, []string{"exec", "--json", "--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true", "--skip-git-repo-check", "-"}) {
		t.Fatalf("a job with no model: %q", got)
	}
}

// An evaluation session's sandboxed commands get the network: without
// `sandbox_workspace_write.network_access=true` Codex's seccomp filter
// refuses socket() and no test that binds loopback can run (ADR 011,
// amendment 2026-09-29).
func TestCodexArgsGiveTheEvaluationSandboxTheNetwork(t *testing.T) {
	args := strings.Join((&Codex{}).Args(Job{Model: "gpt-6-sol", Label: "item ship-it/1 evaluation"}), " ")
	if !strings.Contains(args, "--sandbox workspace-write -c sandbox_workspace_write.network_access=true") {
		t.Fatalf("args %q", args)
	}
}

// The probe runs under exactly the keys the session does and runs a
// command that binds a loopback socket — never `true`, which passes a
// sandbox that refuses socket().
func TestCodexSandboxProbeBindsLoopbackUnderTheSessionsKeys(t *testing.T) {
	got := SandboxProbe([]string{"/usr/bin/mesh", "sandbox-probe"})
	want := []string{"sandbox", "-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.network_access=true", "--", "/usr/bin/mesh", "sandbox-probe"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("probe\n got %q\nwant %q", got, want)
	}
	if exec := strings.Join((&Codex{}).Args(Job{}), " "); !strings.Contains(exec, strings.Join(SandboxNetwork, " ")) {
		t.Fatalf("the session runs %q, not the keys the probe checks", exec)
	}
	cmd := (&Codex{}).probeCommand()
	if len(cmd) != 2 || cmd[1] != "sandbox-probe" || cmd[0] == "true" {
		t.Fatalf("default probe command %q", cmd)
	}
	var out strings.Builder
	if err := ProbeLoopback(&out); err != nil || !strings.HasPrefix(out.String(), "127.0.0.1:") {
		t.Fatalf("ProbeLoopback = %v, %q", err, out.String())
	}
}

// The servers as Codex's config.toml tables: secrets by variable name
// only (ADR 006), their values in the environment, and every server's
// tools approved — `codex exec` refuses an unapproved MCP call under
// workspace-write.
func TestCodexMCPConfig(t *testing.T) {
	toml, env, err := CodexMCPConfig(testServers(), nil)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[mcp_servers.local]
command = "mcp-local"
args = ["--root", "."]
env_vars = ["LOCAL_TOKEN"]
default_tools_approval_mode = "approve"

[mcp_servers.mesh-context]
url = "https://api.test/mcp"
env_http_headers = { "Authorization" = "MESH_CODEX_MCP_1_TOKEN" }
default_tools_approval_mode = "approve"
`
	if toml != want {
		t.Fatalf("config.toml\n%s\nwant\n%s", toml, want)
	}
	if !reflect.DeepEqual(env, []string{"LOCAL_TOKEN=local-secret", "MESH_CODEX_MCP_1_TOKEN=Bearer rs_secret"}) {
		t.Fatalf("env %q", env)
	}
	if toml, env, err := CodexMCPConfig(nil, nil); toml != "" || env != nil || err != nil {
		t.Fatalf("no servers: %q %q %v", toml, env, err)
	}
}

// Codex expands no ${VAR}: a server rendered for Claude Code (a console
// tool's secret as ${MESH_TOOL_…}) has each reference resolved from the
// job's environment, and one the environment does not set is refused —
// never written for Codex to send as a literal header.
func TestCodexMCPConfigResolvesOrRefusesRefs(t *testing.T) {
	servers := map[string]any{
		"search": map[string]any{"type": "http", "url": "https://search.test/mcp",
			"headers": map[string]string{"Authorization": "Bearer ${MESH_TOOL_SEARCH_AUTHORIZATION}"}},
		"fs": map[string]any{"command": "npx", "args": []any{"-y", "fs-mcp"},
			"env": map[string]any{"FS_API_KEY": "${MESH_TOOL_FS_FS_API_KEY}"}},
	}
	jobEnv := []string{"MESH_TOOL_SEARCH_AUTHORIZATION=tok-search", "MESH_TOOL_FS_FS_API_KEY=key-fs"}
	toml, env, err := CodexMCPConfig(servers, jobEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(env, []string{"FS_API_KEY=key-fs", "MESH_CODEX_MCP_1_TOKEN=Bearer tok-search"}) {
		t.Fatalf("env %q", env)
	}
	for _, text := range append([]string{toml}, env...) {
		if strings.Contains(text, "${") {
			t.Fatalf("an unexpanded reference reaches Codex: %q", text)
		}
	}
	if strings.Contains(toml, "tok-search") || strings.Contains(toml, "key-fs") {
		t.Fatalf("a secret value in config.toml:\n%s", toml)
	}
	for _, drop := range jobEnv {
		var partial []string
		for _, kv := range jobEnv {
			if kv != drop {
				partial = append(partial, kv)
			}
		}
		name, _, _ := strings.Cut(drop, "=")
		_, _, err := CodexMCPConfig(servers, partial)
		if err == nil || !strings.Contains(err.Error(), "${"+name+"}") {
			t.Fatalf("without %s: err %v, want a refusal naming it", name, err)
		}
	}
}

// mustCodexConfig is CodexMCPConfig's config.toml of servers.
func mustCodexConfig(t *testing.T, servers map[string]any) string {
	t.Helper()
	toml, _, err := CodexMCPConfig(servers, nil)
	if err != nil {
		t.Fatal(err)
	}
	return toml
}

// fakeCodex is a `codex` that records its stdin, environment and
// CODEX_HOME in out, then replays a fixture.
func fakeCodex(t *testing.T, out, fixture string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"cat > " + out + "/stdin\n" +
		"env > " + out + "/env\n" +
		"pwd > " + out + "/pwd\n" +
		"echo \"$CODEX_HOME\" > " + out + "/home\n" +
		"ls -A \"$CODEX_HOME\" > " + out + "/home.ls\n" +
		"[ -f \"$CODEX_HOME/auth.json\" ] && cp \"$CODEX_HOME/auth.json\" " + out + "/auth.json\n" +
		"[ -f \"$CODEX_HOME/config.toml\" ] && cp \"$CODEX_HOME/config.toml\" " + out + "/config.toml\n" +
		"cat " + abs + "\n"
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func envOf(t *testing.T, path string) map[string][]string {
	seen := map[string][]string{}
	for _, line := range strings.Split(readFile(t, path), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			seen[k] = append(seen[k], v)
		}
	}
	return seen
}

// A plan account: the session's CODEX_HOME is fresh, holds a copy of
// the account's auth.json and nothing else, and is gone after Run; the
// Result is the fixture's, parsed; the prompt came on stdin.
func TestCodexRunPlanAccount(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "operator-key")
	t.Setenv("CODEX_HOME", "/home/operator/.codex")
	out, login, root, ws := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	const auth = `{"auth_mode":"chatgpt","tokens":{"refresh_token":"r1"}}`
	if err := os.WriteFile(filepath.Join(login, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Codex{Binary: fakeCodex(t, out, "codex_exec_apikey.jsonl"), HomeRoot: root,
		Account: CodexAccount{Name: "plus", Home: login}}
	var progress []Progress
	res, err := c.Run(context.Background(), Job{Workspace: ws, Prompt: "say hello", Model: "gpt-6-astra", MCPServers: testServers()},
		func(p Progress) { progress = append(progress, p) })
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success || res.Text != "hello from codex" || res.SessionID != "01a0d7ef-290d-70d1-9303-01b1bf7584bc" ||
		res.Usage == nil || res.Usage.InputTokens != 12624 || res.CostUSD < 0.158 || res.CostUSD > 0.1582 {
		t.Fatalf("result %+v usage %+v", res, res.Usage)
	}
	if len(progress) != 3 || progress[1].Text != "hello from codex" || progress[2].CostUSD != res.CostUSD {
		t.Fatalf("progress %+v", progress)
	}
	if got := readFile(t, filepath.Join(out, "stdin")); got != "say hello" {
		t.Fatalf("stdin %q", got)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(out, "pwd"))); got != ws {
		t.Fatalf("ran in %q, want the workspace %q", got, ws)
	}
	home := strings.TrimSpace(readFile(t, filepath.Join(out, "home")))
	if filepath.Dir(home) != root {
		t.Fatalf("CODEX_HOME %q is not a fresh dir under %q", home, root)
	}
	if got := strings.Fields(readFile(t, filepath.Join(out, "home.ls"))); !reflect.DeepEqual(got, []string{"auth.json", "config.toml"}) {
		t.Fatalf("CODEX_HOME held %q, want auth.json and config.toml", got)
	}
	if want := mustCodexConfig(t, testServers()); readFile(t, filepath.Join(out, "config.toml")) != want {
		t.Fatalf("config.toml %q", readFile(t, filepath.Join(out, "config.toml")))
	}
	if got := readFile(t, filepath.Join(out, "auth.json")); got != auth {
		t.Fatalf("auth.json %q", got)
	}
	env := envOf(t, filepath.Join(out, "env"))
	if got := env["MESH_CODEX_MCP_1_TOKEN"]; len(got) != 1 || got[0] != "Bearer rs_secret" {
		t.Fatalf("the MCP header's variable %q", got)
	}
	if _, ok := env["OPENAI_API_KEY"]; ok {
		t.Fatal("a plan session sees the operator's OPENAI_API_KEY")
	}
	if got := env["CODEX_HOME"]; len(got) != 1 || got[0] != home {
		t.Fatalf("CODEX_HOME in env %q", got)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CODEX_HOME %s is still there after Run: %v", home, err)
	}
	if got := readFile(t, filepath.Join(login, "auth.json")); got != auth {
		t.Fatalf("an unchanged login was rewritten: %q", got)
	}
}

// A session whose job.Env carries CODEX_HOME=<dir> runs there: the
// harness writes only config.toml into it, an auth.json already there
// (the node's own issued login) is untouched, no CODEX_HOME of the
// harness's own reaches the process (the job's wins), and the directory
// is still there — with its auth.json unwritten-back — after Run
// returns: the session that made it owns it, not the harness.
func TestCodexRunJobCodexHome(t *testing.T) {
	out, ws, home := t.TempDir(), t.TempDir(), t.TempDir()
	const auth = `{"auth_mode":"chatgpt","tokens":{"refresh_token":"issued"}}`
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Codex{Binary: fakeCodex(t, out, "codex_exec_apikey.jsonl"), HomeRoot: t.TempDir(),
		Account: CodexAccount{Name: "plus"}}
	res, err := c.Run(context.Background(), Job{Workspace: ws, Prompt: "say hello", Model: "gpt-6-astra",
		MCPServers: testServers(), Env: []string{"CODEX_HOME=" + home}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success || res.Text != "hello from codex" {
		t.Fatalf("result %+v", res)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(out, "home"))); got != home {
		t.Fatalf("CODEX_HOME %q, want the job's %q", got, home)
	}
	if want := mustCodexConfig(t, testServers()); readFile(t, filepath.Join(out, "config.toml")) != want {
		t.Fatalf("config.toml %q", readFile(t, filepath.Join(out, "config.toml")))
	}
	if got := readFile(t, filepath.Join(home, "auth.json")); got != auth {
		t.Fatalf("the job's own auth.json was touched: %q", got)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("CODEX_HOME %s is gone after Run, want it left for its own AccountSession: %v", home, err)
	}
	env := envOf(t, filepath.Join(out, "env"))
	if got := env["CODEX_HOME"]; len(got) != 1 || got[0] != home {
		t.Fatalf("CODEX_HOME in env %q, want exactly the job's", got)
	}
}

// The newest session file under CODEX_HOME/sessions carries the plan's
// rate-limit standing, and Run sets Result.RateLimit from it since the
// exec stream carries none.
func TestCodexRunReadsSessionRateLimits(t *testing.T) {
	root, ws := t.TempDir(), t.TempDir()
	fixture, err := filepath.Abs(filepath.Join("testdata", "codex_session_ratelimits.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := filepath.Abs(filepath.Join("testdata", "codex_exec_apikey.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"mkdir -p \"$CODEX_HOME/sessions/2026/09/28\"\n" +
		"cp " + fixture + " \"$CODEX_HOME/sessions/2026/09/28/rollout-2026-09-28T14-02-01-01a0d802.jsonl\"\n" +
		"cat " + stream + "\n"
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_TEST_CODEX_KEY", "sk-x")
	c := &Codex{Binary: bin, HomeRoot: root, Account: CodexAccount{Name: "api", APIKeyEnv: "MESH_TEST_CODEX_KEY"}}
	res, err := c.Run(context.Background(), Job{Workspace: ws, Prompt: "hi", Model: "gpt-6-astra"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RateLimit == nil {
		t.Fatal("Result.RateLimit is nil, want the session file's reading")
	}
	if res.RateLimit.Status != "allowed" {
		t.Fatalf("status %q, want allowed", res.RateLimit.Status)
	}
	if got := res.RateLimit.FiveHour.Utilization; got != 0.415 {
		t.Fatalf("five-hour utilization %v, want 0.415", got)
	}
	if got := res.RateLimit.SevenDay.Utilization; got != 0.1275 {
		t.Fatalf("seven-day utilization %v, want 0.1275", got)
	}
}

// Codex refreshes a plan's tokens in its home, and the refresh token
// rotates: a changed auth.json is written back to the login home.
func TestCodexRunWritesBackRefreshedLogin(t *testing.T) {
	login, root := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(login, "auth.json"), []byte(`{"r":"1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\ncat >/dev/null\necho '{\"r\":\"2\"}' > \"$CODEX_HOME/auth.json\"\n" +
		`echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Codex{Binary: bin, HomeRoot: root, Account: CodexAccount{Name: "plus", Home: login}}
	if _, err := c.Run(context.Background(), Job{Workspace: t.TempDir(), Prompt: "x"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(login, "auth.json"))); got != `{"r":"2"}` {
		t.Fatalf("login auth.json %q, want the refreshed one", got)
	}
}

// An API-key account: the key is in the session's environment (both
// names Codex reads) and nowhere on disk, the operator's own key never
// reaches it, and the home is gone after Run.
func TestCodexRunAPIKeyAccount(t *testing.T) {
	const key = "sk-proj-ACCOUNTKEY"
	t.Setenv("OPENAI_API_KEY", "operator-key")
	t.Setenv("MESH_TEST_CODEX_KEY", key)
	for _, acct := range []CodexAccount{
		{Name: "api", APIKeyEnv: "MESH_TEST_CODEX_KEY"},
		{Name: "api", APIKeyFile: writeKeyFile(t, key+"\n")},
	} {
		out, root, ws := t.TempDir(), t.TempDir(), t.TempDir()
		var stderr bytes.Buffer
		c := &Codex{Binary: fakeCodex(t, out, "codex_exec_apikey.jsonl"), HomeRoot: root, Stderr: &stderr, Account: acct}
		res, err := c.Run(context.Background(), Job{Workspace: ws, Prompt: "hi", Model: "gpt-6-astra"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Success || res.Text != "hello from codex" {
			t.Fatalf("result %+v", res)
		}
		env := envOf(t, filepath.Join(out, "env"))
		for _, k := range []string{"OPENAI_API_KEY", "CODEX_API_KEY"} {
			if got := env[k]; len(got) != 1 || got[0] != key {
				t.Fatalf("%s = %q, want only the account's key", k, got)
			}
		}
		if got := strings.TrimSpace(readFile(t, filepath.Join(out, "home.ls"))); got != "" {
			t.Fatalf("an API-key home held %q, want nothing", got)
		}
		home := strings.TrimSpace(readFile(t, filepath.Join(out, "home")))
		if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("CODEX_HOME %s is still there after Run: %v", home, err)
		}
		filepath.Walk(ws, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && strings.Contains(readFile(t, p), key) {
				t.Errorf("the key is written to the workspace: %s", p)
			}
			return nil
		})
	}
}

func writeKeyFile(t *testing.T, s string) string {
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A key variable that is empty fails the session before Codex starts,
// and says which variable without a value.
func TestCodexRunAPIKeyMissing(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MESH_TEST_CODEX_KEY", "")
	c := &Codex{Binary: fakeCodex(t, t.TempDir(), "codex_exec_apikey.jsonl"), HomeRoot: root,
		Account: CodexAccount{Name: "api", APIKeyEnv: "MESH_TEST_CODEX_KEY"}}
	_, err := c.Run(context.Background(), Job{Workspace: t.TempDir(), Prompt: "hi"}, nil)
	if err == nil || !strings.Contains(err.Error(), "$MESH_TEST_CODEX_KEY is empty") {
		t.Fatalf("err = %v", err)
	}
	if left, _ := os.ReadDir(root); len(left) != 0 {
		t.Fatalf("a home was left behind: %v", left)
	}
}

// A failed turn is an error with Codex's message, the key redacted.
func TestCodexRunTurnFailed(t *testing.T) {
	t.Setenv("MESH_TEST_CODEX_KEY", "sk-x")
	c := &Codex{Binary: fakeCodex(t, t.TempDir(), "codex_exec_error.jsonl"), HomeRoot: t.TempDir(),
		Account: CodexAccount{Name: "api", APIKeyEnv: "MESH_TEST_CODEX_KEY"}}
	res, err := c.Run(context.Background(), Job{Workspace: t.TempDir(), Prompt: "hi", Model: "no-such-model"}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not exist or you do not have access") {
		t.Fatalf("err = %v", err)
	}
	if res.Success || res.SessionID == "" {
		t.Fatalf("result %+v", res)
	}
}

// A job's Timeout kills the session and Run returns the deadline, and
// the home is still removed.
func TestCodexJobTimeout(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '{\"type\":\"thread.started\",\"thread_id\":\"t\"}'\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_TEST_CODEX_KEY", "sk-x")
	c := &Codex{Binary: bin, HomeRoot: root, Account: CodexAccount{Name: "api", APIKeyEnv: "MESH_TEST_CODEX_KEY"}}
	start := time.Now()
	_, err := c.Run(context.Background(), Job{Workspace: t.TempDir(), Prompt: "x", Timeout: 300 * time.Millisecond}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the session was not killed at the timeout")
	}
	if left, _ := os.ReadDir(root); len(left) != 0 {
		t.Fatalf("a home was left behind: %v", left)
	}
}

// codexEchoSecret is a `codex` that emits a known stdout stream and
// stderr line, both carrying a value job.Secrets should redact.
func codexEchoSecret(t *testing.T) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"echo '{\"type\":\"thread.started\",\"thread_id\":\"t1\"}'\n" +
		"echo '{\"type\":\"item.completed\",\"item\":{\"id\":\"item_1\",\"type\":\"agent_message\",\"text\":\"the key is SEKRIT123\"}}'\n" +
		"echo '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"cached_input_tokens\":0,\"cache_write_input_tokens\":0,\"output_tokens\":1,\"reasoning_output_tokens\":0}}'\n" +
		"echo \"warning: SEKRIT123 nearly leaked\" >&2\n"
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// With Codex.WorkRoot set, Run keeps the session's whole event stream
// and stderr under WorkRoot/kept-sessions/<name>/ — output.jsonl with
// every event line, stderr.log with its stderr, and a state.json-like
// note naming the job's kind, item and goal, model and started-at —
// with every job.Secrets value redacted from both, and never appearing
// at all.
func TestCodexRunKeepsSessionOutput(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MESH_TEST_CODEX_KEY", "sk-x")
	c := &Codex{Binary: codexEchoSecret(t), HomeRoot: t.TempDir(), WorkRoot: root,
		Account: CodexAccount{Name: "api", APIKeyEnv: "MESH_TEST_CODEX_KEY"}}
	job := Job{
		Kind: KindEvaluate, Workspace: t.TempDir(), Prompt: "review it", Model: "gpt-6-sol",
		Secrets: []string{"SEKRIT123"}, Label: "my-goal/3 evaluate",
		Meta: SessionMeta{ItemID: "item-9", GoalID: "goal-9"},
	}
	res, err := c.Run(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success || res.Text != "the key is SEKRIT123" {
		t.Fatalf("result %+v", res)
	}
	if res.KeptSessionDir == "" {
		t.Fatal("Result.KeptSessionDir is empty, want the kept directory")
	}
	if dir := filepath.Dir(res.KeptSessionDir); dir != filepath.Join(root, "kept-sessions") {
		t.Fatalf("kept dir %s is not under %s/kept-sessions", res.KeptSessionDir, root)
	}
	out := readFile(t, filepath.Join(res.KeptSessionDir, sessionunit.OutputFile))
	if !strings.Contains(out, `"thread.started"`) || !strings.Contains(out, `"turn.completed"`) {
		t.Fatalf("output.jsonl %q does not carry the fake's event lines", out)
	}
	if strings.Contains(out, "SEKRIT123") {
		t.Fatalf("output.jsonl carries the secret: %q", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Fatalf("output.jsonl was not redacted: %q", out)
	}
	errLog := readFile(t, filepath.Join(res.KeptSessionDir, sessionunit.StderrFile))
	if !strings.Contains(errLog, "nearly leaked") {
		t.Fatalf("stderr.log %q does not carry the fake's stderr", errLog)
	}
	if strings.Contains(errLog, "SEKRIT123") {
		t.Fatalf("stderr.log carries the secret: %q", errLog)
	}
	var note keptSessionNote
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(res.KeptSessionDir, sessionunit.StateFile))), &note); err != nil {
		t.Fatalf("state.json: %v", err)
	}
	if note.Kind != string(KindEvaluate) || note.ItemID != "item-9" || note.GoalID != "goal-9" || note.Model != "gpt-6-sol" || note.StartedAt.IsZero() {
		t.Fatalf("note %+v", note)
	}
}

// harness.RemoveSession on the job's own work dir never touches the
// kept files: they live under WorkRoot/kept-sessions, not
// sessionunit.Dir(job.Workspace).
func TestCodexRemoveSessionLeavesKeptFilesInPlace(t *testing.T) {
	root, ws := t.TempDir(), t.TempDir()
	t.Setenv("MESH_TEST_CODEX_KEY", "sk-x")
	c := &Codex{Binary: codexEchoSecret(t), HomeRoot: t.TempDir(), WorkRoot: root,
		Account: CodexAccount{Name: "api", APIKeyEnv: "MESH_TEST_CODEX_KEY"}}
	res, err := c.Run(context.Background(), Job{Workspace: ws, Prompt: "review it", Model: "gpt-6-sol"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveSession(ws); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{sessionunit.OutputFile, sessionunit.StderrFile, sessionunit.StateFile} {
		if _, err := os.Stat(filepath.Join(res.KeptSessionDir, f)); err != nil {
			t.Fatalf("%s gone after RemoveSession: %v", f, err)
		}
	}
}

// SweepKeptSessions removes a kept directory whose note is older than
// KeepSessionOutput and leaves a younger one.
func TestSweepKeptSessions(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	mkKept := func(name string, started time.Time) string {
		dir := filepath.Join(root, keptSessionsDir, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(keptSessionNote{Kind: "evaluate", StartedAt: started})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sessionunit.StateFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sessionunit.OutputFile), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	old := mkKept("old-1", now.Add(-25*time.Hour))
	young := mkKept("young-1", now.Add(-1*time.Hour))
	if err := SweepKeptSessions(root, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old kept dir still exists (err %v)", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatalf("young kept dir was removed: %v", err)
	}
}

// A tool's error text is redacted of the job's secrets before it
// leaves Run: it rides the usage report and the item's note.
func TestCodexRunRedactsToolErrors(t *testing.T) {
	t.Setenv("MESH_TEST_CODEX_KEY", "sk-x")
	c := &Codex{Binary: fakeCodex(t, t.TempDir(), "codex_mcp_call_error.jsonl"), HomeRoot: t.TempDir(),
		Account: CodexAccount{Name: "api", APIKeyEnv: "MESH_TEST_CODEX_KEY"}}
	res, err := c.Run(context.Background(), Job{Workspace: t.TempDir(), Prompt: "hi", Model: "gpt-6-sol",
		Secrets: []string{"api.runsheet.dev"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Tools["mesh-context"].FirstError
	if strings.Contains(got, "api.runsheet.dev") || !strings.Contains(got, Redacted) {
		t.Fatalf("FirstError = %q, want the secret redacted", got)
	}
}

// A provider declares the endpoint in the session's config.toml, and
// does it on its own: a session with no MCP server still needs one
// written, or Codex would talk to OpenAI (adr-015).
func TestCodexProviderConfig(t *testing.T) {
	prov := CodexProvider{Name: "kimi", BaseURL: "http://spark-1.lan:8000/v1"}
	cfg, env, err := codexConfig(Job{}, prov)
	if err != nil {
		t.Fatal(err)
	}
	const want = `model_provider = "kimi"

[model_providers.kimi]
name = "kimi"
base_url = "http://spark-1.lan:8000/v1"
wire_api = "chat"
env_key = "OPENAI_API_KEY"
`
	if cfg != want {
		t.Fatalf("config.toml\n%s\nwant\n%s", cfg, want)
	}
	if len(env) != 0 {
		t.Fatalf("a provider needs no environment of its own: %q", env)
	}
	// the MCP servers follow it, one blank line between the tables
	cfg, _, err = codexConfig(Job{MCPServers: testServers()}, prov)
	if err != nil {
		t.Fatal(err)
	}
	if want := want + "\n" + mustCodexConfig(t, testServers()); cfg != want {
		t.Fatalf("the servers do not follow the provider:\n%s\nwant\n%s", cfg, want)
	}
	if !strings.HasPrefix(cfg[len(want):], "\n[mcp_servers.") {
		t.Fatalf("no blank line then the first server table:\n%s", cfg)
	}
	// responses is the other wire; OpenAI itself renders nothing
	if got := (CodexProvider{Name: "x", BaseURL: "http://x/v1", WireAPI: "responses"}).TOML(); !strings.Contains(got, "wire_api = \"responses\"\n") {
		t.Fatalf("wire_api\n%s", got)
	}
	if got := (CodexProvider{}).TOML(); got != "" {
		t.Fatalf("OpenAI itself rendered %q", got)
	}
	if cfg, _, err := codexConfig(Job{}, CodexProvider{}); err != nil || cfg != "" {
		t.Fatalf("OpenAI itself with no server: %q, %v", cfg, err)
	}
	// a name TOML would not take bare is quoted in the table header
	if got := (CodexProvider{Name: "kimi.spark", BaseURL: "http://x/v1"}).TOML(); !strings.Contains(got, "\n[model_providers.\"kimi.spark\"]\n") {
		t.Fatalf("table name\n%s", got)
	}
}

// A provider's own table is the only one its models are priced at; the
// zero provider is OpenAI, priced at CodexPrices (adr-015).
func TestCodexProviderPrice(t *testing.T) {
	kimi := CodexPrice{Input: 0.55, CachedInput: 0.55, CacheWrite: 0.55, Output: 2.20}
	prov := CodexProvider{Name: "kimi", BaseURL: "http://spark-1.lan:8000/v1",
		Prices: map[string]CodexPrice{"kimi-k3": kimi}}
	if p, ok := prov.Price("kimi-k3"); !ok || p != kimi {
		t.Fatalf("kimi-k3 = %+v, %v; want the provider's own price", p, ok)
	}
	if p, ok := prov.Price("gpt-6-sol"); ok {
		t.Fatalf("gpt-6-sol on a provider priced at %+v, want unpriced", p)
	}
	if p, ok := (CodexProvider{}).Price("gpt-6-sol"); !ok || p != CodexPrices["gpt-6-sol"] {
		t.Fatalf("OpenAI's gpt-6-sol = %+v, %v", p, ok)
	}
	if _, ok := (CodexProvider{}).Price("kimi-k3"); ok {
		t.Fatal("OpenAI priced kimi-k3")
	}
}

// A provider account's session: its home's config.toml declares the
// provider though the job has no MCP server, and the Result is costed
// at the provider's table alone — a model absent from it costs nothing
// and is Unpriced, even one OpenAI's table names (adr-015).
func TestCodexRunPricesAtTheProvider(t *testing.T) {
	t.Setenv("MESH_TEST_KIMI_KEY", "kimi-key")
	kimi := CodexPrice{Input: 0.55, CachedInput: 0.55, CacheWrite: 0.55, Output: 2.20}
	prov := CodexProvider{Name: "kimi", BaseURL: "http://spark-1.lan:8000/v1",
		Prices: map[string]CodexPrice{"kimi-k3": kimi}}
	// the fixture's last turn.completed
	usage := CodexUsage{InputTokens: 12624, CacheWriteInputTokens: 12621, OutputTokens: 8}
	for _, tc := range []struct {
		model    string
		cost     float64
		unpriced bool
	}{
		{"gpt-6-sol", 0, true},
		{"kimi-k3", kimi.Cost(usage), false},
	} {
		out := t.TempDir()
		c := &Codex{Binary: fakeCodex(t, out, "codex_exec_apikey.jsonl"), HomeRoot: t.TempDir(),
			Account: CodexAccount{Name: "kimi", APIKeyEnv: "MESH_TEST_KIMI_KEY", Provider: prov}}
		var progress []float64
		res, err := c.Run(context.Background(), Job{Workspace: t.TempDir(), Prompt: "hi", Model: tc.model},
			func(p Progress) {
				if p.CostUSD != 0 {
					progress = append(progress, p.CostUSD)
				}
			})
		if err != nil {
			t.Fatal(err)
		}
		if res.Usage == nil || res.CostUSD != tc.cost || res.Usage.Unpriced != tc.unpriced || res.Usage.InputTokens != 12624 {
			t.Fatalf("%s: cost %v usage %+v, want cost %v unpriced %v", tc.model, res.CostUSD, res.Usage, tc.cost, tc.unpriced)
		}
		if tc.unpriced && len(progress) != 0 || !tc.unpriced && (len(progress) != 1 || progress[0] != tc.cost) {
			t.Fatalf("%s: progress costs %v", tc.model, progress)
		}
		if got, want := readFile(t, filepath.Join(out, "config.toml")), prov.TOML(); got != want {
			t.Fatalf("%s: config.toml\n%s\nwant\n%s", tc.model, got, want)
		}
	}
	// the stream parsed on its own is OpenAI's: gpt-6-sol has a price there
	f, err := os.Open(filepath.Join("testdata", "codex_exec_apikey.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if res := ParseCodexStream(f, "gpt-6-sol", nil); res.Usage.Unpriced || res.CostUSD != CodexPrices["gpt-6-sol"].Cost(usage) {
		t.Fatalf("ParseCodexStream: cost %v usage %+v", res.CostUSD, res.Usage)
	}
}

// A job that names its own CODEX_HOME runs on an issued credential —
// OpenAI's, naming no provider — so the harness's own account's
// provider is not declared there: an issued login pointed at the
// node's local endpoint would never authenticate (adr-015).
func TestCodexRunJobCodexHomeTakesNoProvider(t *testing.T) {
	prov := CodexProvider{Name: "kimi", BaseURL: "http://spark-1.lan:8000/v1"}
	for _, servers := range []map[string]any{testServers(), nil} {
		out, home := t.TempDir(), t.TempDir()
		c := &Codex{Binary: fakeCodex(t, out, "codex_exec_apikey.jsonl"), HomeRoot: t.TempDir(),
			Account: CodexAccount{Name: "kimi", APIKeyEnv: "MESH_TEST_KIMI_KEY", Provider: prov}}
		if _, err := c.Run(context.Background(), Job{Workspace: t.TempDir(), Prompt: "hi", Model: "gpt-6-sol",
			MCPServers: servers, Env: []string{"CODEX_HOME=" + home}}, nil); err != nil {
			t.Fatal(err)
		}
		cfg, err := os.ReadFile(filepath.Join(home, "config.toml"))
		if servers == nil {
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("no server and no provider wrote config.toml %q (%v)", cfg, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(cfg), "model_provider") {
			t.Fatalf("the issued home's config.toml declares a provider:\n%s", cfg)
		}
		if want := mustCodexConfig(t, testServers()); string(cfg) != want {
			t.Fatalf("config.toml\n%s\nwant\n%s", cfg, want)
		}
	}
}
