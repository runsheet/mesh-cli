package harness

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The argv for a job with a model and MCP servers: exec in JSON, the
// workspace-write sandbox, the model, each server as `-c` overrides
// with its headers named by variable, and `-` for the prompt on stdin.
// Neither the prompt nor a header's value is on it.
func TestCodexArgs(t *testing.T) {
	c := &Codex{ExtraArgs: []string{"--skip-git-repo-check"}}
	job := Job{
		Prompt: "build the thing",
		Model:  "gpt-6-sol",
		MCPServers: map[string]any{
			"prologue": map[string]any{"type": "http", "url": "https://api.test/mcp",
				"headers": map[string]string{"Authorization": "Bearer rs_secret"}},
			"local": map[string]any{"command": "mcp-local", "args": []any{"--root", "."}, "env": map[string]any{"MODE": "ro"}},
		},
	}
	want := []string{
		"exec", "--json", "--sandbox", "workspace-write",
		"--model", "gpt-6-sol",
		"-c", `mcp_servers.local.command="mcp-local"`,
		"-c", `mcp_servers.local.args=["--root","."]`,
		"-c", `mcp_servers.local.env={"MODE"="ro"}`,
		"-c", `mcp_servers.prologue.url="https://api.test/mcp"`,
		"-c", `mcp_servers.prologue.env_http_headers={"Authorization"="MESH_CODEX_MCP_0_TOKEN"}`,
		"--skip-git-repo-check",
		"-",
	}
	got := c.Args(job)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args\n got %q\nwant %q", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, "build the thing") || strings.Contains(a, "rs_secret") {
			t.Fatalf("argv carries the prompt or a header value: %q", a)
		}
	}
	if _, env := CodexMCPArgs(job.MCPServers); !reflect.DeepEqual(env, []string{"MESH_CODEX_MCP_0_TOKEN=Bearer rs_secret"}) {
		t.Fatalf("mcp env %q", env)
	}
	if got := c.Args(Job{Prompt: "hi"}); !reflect.DeepEqual(got, []string{"exec", "--json", "--sandbox", "workspace-write", "--skip-git-repo-check", "-"}) {
		t.Fatalf("a job with no model or servers: %q", got)
	}
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
	res, err := c.Run(context.Background(), Job{Workspace: ws, Prompt: "say hello", Model: "gpt-6-astra"},
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
	if got := strings.TrimSpace(readFile(t, filepath.Join(out, "home.ls"))); got != "auth.json" {
		t.Fatalf("CODEX_HOME held %q, want only auth.json", got)
	}
	if got := readFile(t, filepath.Join(out, "auth.json")); got != auth {
		t.Fatalf("auth.json %q", got)
	}
	env := envOf(t, filepath.Join(out, "env"))
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
