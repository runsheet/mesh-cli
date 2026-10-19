package harness

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// codexSecret is the test's credential: an auth.json token, a tool's
// secret and the runsheet bearer all carry it, and it must be in no
// argument and no log line.
const codexSecret = "sk-codex-test-secret-0123456789"

// codexStream is a Codex session that completes its turn, saying so.
const codexStream = `{"type":"thread.started","thread_id":"t1"}
{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"VERDICT: PASS"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}
`

// podmanCall is one `podman` invocation the stub saw.
type podmanCall struct {
	args, env []string
}

// codexPodman is a Podman whose Exec records every call and answers the
// agent's `run -i` with the stream, after writing the secret to its
// stderr split across writes, as a pipe may deliver it, and reading the files the session's CODEX_HOME holds.
func codexPodman(stdin *[]byte, files map[string][]byte) (*sandbox.Podman, func() []podmanCall) {
	var mu sync.Mutex
	var calls []podmanCall
	p := &sandbox.Podman{Exec: func(_ context.Context, args, env []string, in io.Reader, stdout, stderr io.Writer) error {
		mu.Lock()
		calls = append(calls, podmanCall{append([]string(nil), args...), append([]string(nil), env...)})
		mu.Unlock()
		if len(args) > 1 && args[0] == "run" && args[1] == "-i" && slices.Contains(args, "exec") {
			*stdin, _ = io.ReadAll(in)
			home := envValue(env, "CODEX_HOME")
			for _, f := range []string{"config.toml", "auth.json"} {
				files[f], _ = os.ReadFile(filepath.Join(home, f))
			}
			half := len(codexSecret) / 2
			io.WriteString(stderr, "codex: using token "+codexSecret[:3])
			io.WriteString(stderr, codexSecret[3:half])
			io.WriteString(stderr, codexSecret[half:]+"\n")
			io.WriteString(stderr, "codex: and ends on "+codexSecret[:half])
			io.WriteString(stdout, codexStream)
			return nil
		}
		if in != nil {
			_, _ = io.Copy(io.Discard, in)
		}
		return nil
	}}
	return p, func() []podmanCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]podmanCall(nil), calls...)
	}
}

func envValue(env []string, name string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == name {
			v = val
		}
	}
	return v
}

// allowedInPod says a variable may be named on the agent's command line:
// the job's allow-list, the sandbox's own, a tool's, or a limit's.
func allowedInPod(name string, l sandbox.Limits) bool {
	if sandbox.Allowed(name) || slices.Contains(sandbox.BaseEnv, name) {
		return true
	}
	for _, kv := range sandbox.LimitEnv(l) {
		if n, _, _ := strings.Cut(kv, "="); n == name {
			return true
		}
	}
	return false
}

// With a sandbox, a Codex job runs the image's codex in the session's
// pod: `exec --json --sandbox workspace-write` with the prompt on stdin,
// the workspace and the session HOME the only mounts, every variable
// passed by name from the allow-list, CODEX_HOME inside the HOME, and
// the credential in no argument and no log.
func TestCodexRunsInTheSandbox(t *testing.T) {
	var stdin []byte
	files := map[string][]byte{}
	p, calls := codexPodman(&stdin, files)
	limits := sandbox.Limits{CPUs: 2, Memory: "4g", Pids: 512}
	sb := &sandbox.Sandbox{Runtime: p, TempDir: t.TempDir(), Config: sandbox.Config{Image: "ghcr.io/runsheet/mesh-node:v1", Limits: limits}}
	var log bytes.Buffer
	keeper := &rotatingKeeper{}
	c := &Codex{Binary: "/nonexistent/codex", Sandbox: sb, Stderr: &log, Tokens: keeper}

	// the issued credential's home, as PrepareCodexAccount places it
	placed := t.TempDir()
	auth := `{"OPENAI_API_KEY":null,"tokens":{"access_token":"` + codexSecret + `","refresh_token":"rt-` + codexSecret + `"}}`
	if err := os.WriteFile(filepath.Join(placed, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := fullClone(t) // no git common dir to mount
	job := Job{Kind: KindEvaluate, Label: "g/7", Workspace: ws, Prompt: "evaluate it", Model: "gpt-6-sol",
		Env: []string{"CODEX_HOME=" + placed, "MESH_ACCOUNT=codex-pro", "GH_TOKEN=ghs_" + codexSecret,
			"MESH_TOOL_X_TOKEN=" + codexSecret, "NOT_ALLOWED=1"},
		Secrets:    []string{"ghs_" + codexSecret, codexSecret},
		MCPServers: map[string]any{BuiltinMCP: MeshContextRelay("mesh", relayURL, "/unused")},
	}
	res, err := c.Run(context.Background(), job, nil)
	if err != nil || !res.Success || !strings.Contains(res.Text, "VERDICT: PASS") || res.SessionID != "t1" {
		t.Fatalf("run: %+v %v", res, err)
	}
	if string(stdin) != "evaluate it" {
		t.Fatalf("stdin = %q", stdin)
	}

	var agent *podmanCall
	all := calls()
	for i := range all {
		if a := all[i].args; len(a) > 1 && a[0] == "run" && a[1] == "-i" && slices.Contains(a, "exec") {
			agent = &all[i]
		}
	}
	if agent == nil {
		t.Fatalf("no agent run among %v", all)
	}
	args := agent.args
	img := slices.Index(args, sb.Config.Image)
	if img < 0 || img+1 >= len(args) {
		t.Fatalf("no image in %v", args)
	}
	cmd := args[img+1:]
	if cmd[0] != "codex" || cmd[1] != "exec" || !slices.Contains(cmd, "--json") ||
		flagAt(cmd, "--sandbox") != "workspace-write" || cmd[len(cmd)-1] != "-" || flagAt(cmd, "--model") != "gpt-6-sol" {
		t.Fatalf("command = %v", cmd)
	}

	var mounts []string
	for i, a := range args {
		if (a == "--volume" || a == "--mount" || a == "-v") && i+1 < len(args) {
			mounts = append(mounts, args[i+1])
		}
	}
	codexHome := envValue(agent.env, "CODEX_HOME")
	home := filepath.Dir(codexHome)
	if len(mounts) != 2 || !slices.Contains(mounts, ws+":"+ws+":rw") || !slices.Contains(mounts, home+":"+home+":rw") {
		t.Fatalf("mounts = %v, want the workspace and the session HOME %s", mounts, home)
	}
	if !strings.HasPrefix(home, sb.TempDir) || codexHome != filepath.Join(home, ".codex") || envValue(agent.env, "HOME") != home {
		t.Fatalf("CODEX_HOME %s is not under the session HOME %s", codexHome, home)
	}

	var names []string
	for i, a := range args {
		if a == "--env" && i+1 < len(args) {
			n := args[i+1]
			if strings.Contains(n, "=") {
				t.Fatalf("--env %s carries a value", n)
			}
			if !allowedInPod(n, limits) {
				t.Fatalf("--env %s is not on the allow-list", n)
			}
			names = append(names, n)
		}
	}
	for _, want := range []string{"CODEX_HOME", "MESH_ACCOUNT", "MESH_ITEM", "MESH_SESSION_KIND", "GH_TOKEN", "MESH_TOOL_X_TOKEN", "GOMAXPROCS"} {
		if !slices.Contains(names, want) {
			t.Fatalf("--env %s missing from %v", want, names)
		}
	}
	if slices.Contains(names, "NOT_ALLOWED") {
		t.Fatalf("a variable off the allow-list reached the pod: %v", names)
	}

	for _, c := range all {
		for _, a := range c.args {
			if strings.Contains(a, codexSecret) || strings.Contains(a, bearerPrefix) {
				t.Fatalf("a secret is on podman's command line: %v", c.args)
			}
		}
	}
	if strings.Contains(log.String(), codexSecret) || !strings.Contains(log.String(), Redacted) {
		t.Fatalf("the log is not redacted: %q", log.String())
	}
	// the credential is a file in the HOME; the bearer stays on the node,
	// the relay naming its endpoint's socket in the HOME
	if string(files["auth.json"]) != auth {
		t.Fatalf("auth.json in CODEX_HOME = %q", files["auth.json"])
	}
	cfg := string(files["config.toml"])
	if !strings.Contains(cfg, "[mcp_servers.runsheet]") || !strings.Contains(cfg, filepath.Join(home, relayScriptName)) ||
		!strings.Contains(cfg, filepath.Join(home, RelaySocketName)) || strings.Contains(cfg, TokenFileName) || strings.Contains(cfg, bearerPrefix) {
		t.Fatalf("config.toml = %s", cfg)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("the session HOME outlived the session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(placed, "auth.json")); err != nil {
		t.Fatalf("the placed login is gone: %v", err)
	}
}

// An API-key account's key reaches the pod by name only: never on the
// argv, never in config.toml, never in the log.
func TestCodexSandboxAPIKeyByName(t *testing.T) {
	var stdin []byte
	files := map[string][]byte{}
	p, calls := codexPodman(&stdin, files)
	sb := &sandbox.Sandbox{Runtime: p, TempDir: t.TempDir(), Config: sandbox.Config{Image: "img"}}
	t.Setenv("MESH_TEST_CODEX_KEY", codexSecret)
	var log bytes.Buffer
	c := &Codex{Sandbox: sb, Stderr: &log, Account: CodexAccount{Name: "key", APIKeyEnv: "MESH_TEST_CODEX_KEY"}}
	if _, err := c.Run(context.Background(), Job{Kind: KindEvaluate, Label: "g/7", Workspace: fullClone(t), Prompt: "p"}, nil); err != nil {
		t.Fatal(err)
	}
	var agent podmanCall
	for _, c := range calls() {
		for _, a := range c.args {
			if strings.Contains(a, codexSecret) {
				t.Fatalf("the key is on podman's command line: %v", c.args)
			}
		}
		if slices.Contains(c.args, "exec") {
			agent = c
		}
	}
	if envValue(agent.env, "CODEX_API_KEY") != codexSecret || envValue(agent.env, "OPENAI_API_KEY") != codexSecret ||
		!slices.Contains(agent.args, "CODEX_API_KEY") {
		t.Fatalf("the key does not reach the pod by name: %v", agent.args)
	}
	if strings.Contains(string(files["config.toml"]), codexSecret) || strings.Contains(log.String(), codexSecret) {
		t.Fatalf("the key leaked: config %q, log %q", files["config.toml"], log.String())
	}
}

// A pod its limits killed comes back as the *sandbox.LimitError.
func TestCodexReturnsASandboxLimitKill(t *testing.T) {
	f := &sandbox.Fake{Agent: func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		io.WriteString(spec.Stdout, `{"type":"thread.started","thread_id":"t1"}`+"\n")
		return sandbox.Exit{Code: 137, OOMKilled: true}, nil
	}}
	sb := &sandbox.Sandbox{Runtime: f, TempDir: t.TempDir(), Config: sandbox.Config{Image: "img", Limits: sandbox.Limits{Memory: "6g"}}}
	placed := t.TempDir()
	os.WriteFile(filepath.Join(placed, "auth.json"), []byte(`{"tokens":{}}`), 0o600)
	_, err := (&Codex{Sandbox: sb}).Run(context.Background(), Job{Workspace: fullClone(t), Prompt: "p", Env: []string{"CODEX_HOME=" + placed}}, nil)
	var le *sandbox.LimitError
	if !errors.As(err, &le) || le.Limit != "memory" {
		t.Fatalf("err = %v", err)
	}
}

// A sandboxed job with no placed home and no account is refused, as on
// the host: never the operator's own login.
func TestCodexSandboxNeedsAnIssuedCredential(t *testing.T) {
	f := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: f, TempDir: t.TempDir(), Config: sandbox.Config{Image: "img"}}
	_, err := (&Codex{Sandbox: sb}).Run(context.Background(), Job{Workspace: fullClone(t), Prompt: "p"}, nil)
	if !errors.Is(err, ErrNoIssuedCredential) || len(f.Agents) != 0 {
		t.Fatalf("err = %v, agents %d", err, len(f.Agents))
	}
}

// fullClone is a workspace that is a full clone: its own .git directory,
// so no common dir is mounted beside it.
func fullClone(t *testing.T) string {
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}
