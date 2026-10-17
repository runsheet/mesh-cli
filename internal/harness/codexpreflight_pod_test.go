package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// preflightPodAgent is a fake pod's containers as a Codex session's preflight and
// run use them: `codex mcp get` answers, the runsheet relay (node) answers
// an MCP opening on its stdio — or refuses auth with refuse — the sandbox
// probe binds, and `codex exec` streams a verdict. Every token file the
// relay was named must exist when it runs.
func preflightPodAgent(t *testing.T, refuse bool) func(context.Context, sandbox.AgentSpec) (sandbox.Exit, error) {
	return func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		cmd := spec.Command
		switch {
		case len(cmd) >= 4 && cmd[0] == "codex" && cmd[1] == "mcp" && cmd[2] == "get":
			fmt.Fprintf(spec.Stdout, "%s\n  enabled: true\n", cmd[3])
		case len(cmd) == 4 && cmd[0] == "node":
			if _, err := os.Stat(cmd[3]); err != nil {
				t.Errorf("the relay's token file is not there: %v", err)
			}
			sc := bufio.NewScanner(spec.Stdin)
			for sc.Scan() {
				var m struct {
					ID     *int   `json:"id"`
					Method string `json:"method"`
				}
				if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil {
					continue
				}
				switch {
				case refuse:
					fmt.Fprintf(spec.Stdout, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"runsheet relay: %s answered 401: no"}}`+"\n", *m.ID, cmd[2])
				case m.Method == "initialize":
					fmt.Fprintf(spec.Stdout, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18"}}`+"\n", *m.ID)
				case m.Method == "tools/list":
					fmt.Fprintf(spec.Stdout, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"mesh_search"}]}}`+"\n", *m.ID)
				}
			}
		case len(cmd) > 1 && cmd[0] == "codex" && cmd[1] == "sandbox":
			fmt.Fprintln(spec.Stdout, "127.0.0.1:41234")
		case len(cmd) > 1 && cmd[0] == "codex" && cmd[1] == "exec":
			_, _ = io.Copy(io.Discard, spec.Stdin)
			io.WriteString(spec.Stdout, codexStream)
		default:
			return sandbox.Exit{Code: 127}, nil
		}
		return sandbox.Exit{}, nil
	}
}

// podCodexJob is an evaluation's Codex job on an issued credential's home,
// with the runsheet relay.
func podCodexJob(t *testing.T) Job {
	placed := t.TempDir()
	auth := `{"tokens":{"access_token":"` + codexSecret + `"}}`
	if err := os.WriteFile(filepath.Join(placed, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	return Job{Kind: KindEvaluate, Label: "g/7 evaluate", Workspace: fullClone(t), Prompt: "evaluate it", Model: "gpt-6-sol",
		Env:        []string{"CODEX_HOME=" + placed, "MESH_ACCOUNT=codex-pro", "GH_TOKEN=ghs_" + codexSecret, "NOT_ALLOWED=1"},
		Secrets:    []string{"ghs_" + codexSecret, codexSecret},
		MCPServers: map[string]any{BuiltinMCP: MeshContextRelay("mesh", relayURL, "/unused")},
	}
}

func envNames(env []string) []string {
	var out []string
	for _, kv := range env {
		n, _, _ := strings.Cut(kv, "=")
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// On a sandboxed node the Codex preflight runs in the session's own pod
// — the one the node hands the run — before the session: the image's
// `codex mcp get runsheet`, the relay's tools/list handshake and the
// sandbox probe, each in that pod with CODEX_HOME inside the session's
// HOME and the same variables by name as the session's `codex exec`.
func TestCodexPreflightRunsInTheSessionsPod(t *testing.T) {
	f := &sandbox.Fake{Agent: preflightPodAgent(t, false)}
	sb := &sandbox.Sandbox{Runtime: f, TempDir: t.TempDir(), Config: sandbox.Config{Image: "img"}}
	c := &Codex{Binary: "/nonexistent/codex", Sandbox: sb, Tokens: &rotatingKeeper{}}
	sess, err := sb.Open("g/7 evaluate", string(KindEvaluate), sandbox.GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	job := podCodexJob(t)
	job.Session = sess

	if err := c.Preflight(context.Background(), job); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if res, err := c.Run(context.Background(), job, nil); err != nil || !res.Success {
		t.Fatalf("run: %+v %v", res, err)
	}

	if len(f.Pods) != 1 {
		t.Fatalf("pods = %d, want the one session pod shared by preflight and run", len(f.Pods))
	}
	pod := sandbox.Pod{Name: f.Pods[0].Name}
	if len(f.Agents) != 4 {
		t.Fatalf("containers = %d: %+v", len(f.Agents), f.Agents)
	}
	want := []struct {
		name string
		cmd  []string
	}{
		{sandbox.ExecName(pod, 1), []string{"codex", "mcp", "get", BuiltinMCP}},
		{sandbox.ExecName(pod, 2), []string{"node", filepath.Join(sess.Home, relayScriptName), relayURL, filepath.Join(sess.CodexHome(), TokenFileName)}},
		{sandbox.ExecName(pod, 3), append([]string{"codex"}, SandboxProbe(PodProbeLoopback)...)},
		{sandbox.AgentName(pod), append([]string{"codex"}, c.Args(job)...)},
	}
	session := envNames(f.Agents[3].Env)
	for i, w := range want {
		a := f.Agents[i]
		if a.Name != w.name || !slices.Equal(a.Command, w.cmd) {
			t.Fatalf("container %d = %s %v, want %s %v", i, a.Name, a.Command, w.name, w.cmd)
		}
		if a.Image != "img" || a.Workdir != job.Workspace {
			t.Fatalf("container %d runs %s in %s", i, a.Image, a.Workdir)
		}
		if got := envValue(a.Env, "CODEX_HOME"); got != sess.CodexHome() || !strings.HasPrefix(got, sess.Home+string(filepath.Separator)) {
			t.Fatalf("container %d's CODEX_HOME %s is not inside the session HOME %s", i, got, sess.Home)
		}
		if envValue(a.Env, "HOME") != sess.Home {
			t.Fatalf("container %d's HOME = %s", i, envValue(a.Env, "HOME"))
		}
		if got := envNames(a.Env); !slices.Equal(got, session) {
			t.Fatalf("container %d's env %v, the session's %v", i, got, session)
		}
		for _, arg := range a.Command {
			if strings.Contains(arg, codexSecret) || strings.Contains(arg, bearerPrefix) {
				t.Fatalf("a secret is on container %d's command line: %v", i, a.Command)
			}
		}
		if len(a.Mounts) != 2 {
			t.Fatalf("container %d mounts %v", i, a.Mounts)
		}
	}
	if slices.Contains(session, "NOT_ALLOWED") || !slices.Contains(session, "MESH_ACCOUNT") {
		t.Fatalf("the session's env %v", session)
	}
}

// A relay in the pod that refuses the bearer fails the preflight with the
// host's wording, and no session runs in the pod.
func TestCodexPreflightInThePodRefusesAuth(t *testing.T) {
	f := &sandbox.Fake{Agent: preflightPodAgent(t, true)}
	sb := &sandbox.Sandbox{Runtime: f, TempDir: t.TempDir(), Config: sandbox.Config{Image: "img"}}
	c := &Codex{Sandbox: sb, Tokens: &rotatingKeeper{}}
	err := c.Preflight(context.Background(), podCodexJob(t))
	if err == nil || !strings.HasPrefix(err.Error(), "refuses auth: ") || strings.Contains(err.Error(), codexSecret) {
		t.Fatalf("err = %v", err)
	}
	for _, a := range f.Agents {
		if slices.Contains(a.Command, "exec") || slices.Contains(a.Command, "sandbox") {
			t.Fatalf("ran %v after a refused handshake", a.Command)
		}
	}
	// the harness's own session (no job.Session) is closed: its pod removed
	if len(f.Pods) != 1 || len(f.Removed) != 1 {
		t.Fatalf("pods %d removed %v", len(f.Pods), f.Removed)
	}
}

// `codex mcp get` failing in the pod names the step and its output.
func TestCodexPreflightInThePodMCPGetFails(t *testing.T) {
	var mu sync.Mutex
	f := &sandbox.Fake{Agent: func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		mu.Lock()
		defer mu.Unlock()
		io.WriteString(spec.Stderr, "Error: No MCP server named 'runsheet' found.\n")
		return sandbox.Exit{Code: 1}, nil
	}}
	sb := &sandbox.Sandbox{Runtime: f, TempDir: t.TempDir(), Config: sandbox.Config{Image: "img"}}
	c := &Codex{Sandbox: sb, Tokens: &rotatingKeeper{}}
	err := c.Preflight(context.Background(), podCodexJob(t))
	if err == nil || err.Error() != "codex mcp get runsheet: Error: No MCP server named 'runsheet' found." {
		t.Fatalf("err = %v", err)
	}
	if len(f.Agents) != 1 {
		t.Fatalf("containers = %d", len(f.Agents))
	}
}

// A sandbox probe refused a socket in the pod is `sandbox: <the line>`.
func TestCodexPreflightInThePodProbeRefusesSocket(t *testing.T) {
	f := &sandbox.Fake{Agent: func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		if spec.Command[1] == "sandbox" {
			io.WriteString(spec.Stderr, "bind: operation not permitted\n")
			return sandbox.Exit{Code: 1}, nil
		}
		return sandbox.Exit{}, nil
	}}
	sb := &sandbox.Sandbox{Runtime: f, TempDir: t.TempDir(), Config: sandbox.Config{Image: "img"}}
	job := podCodexJob(t)
	job.MCPServers = nil
	err := (&Codex{Sandbox: sb}).Preflight(context.Background(), job)
	if err == nil || err.Error() != "sandbox: bind: operation not permitted" {
		t.Fatalf("err = %v", err)
	}
}
