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
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// With a sandbox, claude runs in the session's pod: the prompt on its
// stdin, the stream read from its stdout, the MCP config a file under
// the temporary HOME rather than inline, and no host binary looked up.
func TestClaudeCodeRunsInTheSandbox(t *testing.T) {
	var mcpFile, script []byte
	var stdin []byte
	f := &sandbox.Fake{Agent: func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		stdin, _ = io.ReadAll(spec.Stdin)
		if p := flagAt(spec.Command, "--mcp-config"); p != "" {
			mcpFile, _ = os.ReadFile(p)
			script, _ = os.ReadFile(filepath.Join(filepath.Dir(p), workScriptName))
		}
		io.WriteString(spec.Stdout, `{"type":"system","subtype":"init","session_id":"s1"}`+"\n")
		io.WriteString(spec.Stdout, `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1","total_cost_usd":0.2}`+"\n")
		return sandbox.Exit{}, nil
	}}
	sb := &sandbox.Sandbox{Runtime: f, TempDir: t.TempDir(), Config: sandbox.Config{Image: "img"}}
	c := &ClaudeCode{Binary: "/nonexistent/claude", ExtraArgs: []string{"--dangerously-skip-permissions"}, Sandbox: sb}
	ws := t.TempDir()
	job := Job{Kind: KindBuild, Label: "g/1", Workspace: ws, Prompt: "build it", Model: "opus",
		Env:        []string{"GH_TOKEN=ghs_x", "NOT_ALLOWED=1"},
		MCPServers: map[string]any{"runsheet": map[string]any{"type": "http", "url": "https://gw/mcp", "headers": map[string]string{"Authorization": "Bearer rs_secret"}}}}
	res, err := c.Run(context.Background(), job, nil)
	if err != nil || !res.Success || res.Text != "done" || res.SessionID != "s1" {
		t.Fatalf("run: %+v %v", res, err)
	}
	if string(stdin) != "build it" {
		t.Fatalf("stdin = %q", stdin)
	}
	spec := f.Agents[0]
	if spec.Command[0] != "claude" || spec.Workdir != ws {
		t.Fatalf("command %v in %s", spec.Command, spec.Workdir)
	}
	mcpPath := flagAt(spec.Command, "--mcp-config")
	home := filepath.Dir(mcpPath)
	if !strings.HasPrefix(mcpPath, sb.TempDir) || !strings.Contains(string(mcpFile), "rs_secret") {
		t.Fatalf("mcp config %s = %s", mcpPath, mcpFile)
	}
	for _, a := range spec.Command {
		if strings.Contains(a, "rs_secret") {
			t.Fatalf("the runsheet bearer is on the command line: %v", spec.Command)
		}
	}
	if spec.Command[len(spec.Command)-1] != "--dangerously-skip-permissions" || flagAt(spec.Command, "--allowedTools") != "mcp__mesh-node__*,mcp__runsheet__*" ||
		flagAt(spec.Command, "--disallowedTools") != "mcp__runsheet__mesh_finish,mcp__runsheet__mesh_fail" {
		t.Fatalf("command = %v", spec.Command)
	}
	// a build's own finish tools: node running the script beside mcp.json
	if !strings.Contains(string(mcpFile), `"mesh-node"`) || !strings.Contains(string(mcpFile), filepath.Join(home, workScriptName)) || !bytes.Equal(script, WorkScript()) {
		t.Fatalf("mcp config %s, script %d bytes", mcpFile, len(script))
	}
	if !slices.Contains(spec.Env, "CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude")) || slices.Contains(spec.Env, "NOT_ALLOWED=1") {
		t.Fatalf("env = %v", spec.Env)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("the temp HOME outlived the session: %v", err)
	}
}

// A pod its limits killed comes back as the *sandbox.LimitError.
func TestClaudeCodeReturnsASandboxLimitKill(t *testing.T) {
	f := &sandbox.Fake{Agent: func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		io.WriteString(spec.Stdout, `{"type":"system","subtype":"init","session_id":"s1"}`+"\n")
		return sandbox.Exit{Code: 137, OOMKilled: true}, nil
	}}
	sb := &sandbox.Sandbox{Runtime: f, TempDir: t.TempDir(), Config: sandbox.Config{Image: "img", Limits: sandbox.Limits{Memory: "6g"}}}
	_, err := (&ClaudeCode{Sandbox: sb}).Run(context.Background(), Job{Workspace: t.TempDir(), Prompt: "p"}, nil)
	var le *sandbox.LimitError
	if !errors.As(err, &le) || le.Limit != "memory" {
		t.Fatalf("err = %v", err)
	}
}

func flagAt(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// SandboxBinary replaces `claude` as the agent's command in the pod (the
// e2e's stub agent, in the workspace); the host Binary still does not.
func TestClaudeCodeSandboxBinary(t *testing.T) {
	f := &sandbox.Fake{Agent: func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		io.WriteString(spec.Stdout, `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1"}`+"\n")
		return sandbox.Exit{}, nil
	}}
	sb := &sandbox.Sandbox{Runtime: f, TempDir: t.TempDir(), Config: sandbox.Config{Image: "img"}}
	ws := t.TempDir()
	stub := filepath.Join(ws, ".mesh-e2e-agent")
	c := &ClaudeCode{Binary: "/nonexistent/claude", SandboxBinary: stub, Sandbox: sb}
	if _, err := c.Run(context.Background(), Job{Kind: KindBuild, Label: "g/1", Workspace: ws, Prompt: "p"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.Agents[0].Command[0]; got != stub {
		t.Fatalf("command = %s, want %s", got, stub)
	}
}
