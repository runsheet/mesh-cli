package node

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// codexPodRuntime is a fake runtime whose containers answer as the node
// image's would for a Codex evaluation: `codex mcp get` (failing with
// mcpGetFails), the runsheet relay's MCP opening on its stdio, the
// sandbox probe, and `codex exec` streaming codexSays.
func codexPodRuntime(mcpGetFails bool, codexSays string) *sandbox.Fake {
	var mu sync.Mutex
	return &sandbox.Fake{Agent: func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		mu.Lock()
		defer mu.Unlock()
		cmd := spec.Command
		switch {
		case len(cmd) > 2 && cmd[0] == "codex" && cmd[1] == "mcp":
			if mcpGetFails {
				io.WriteString(spec.Stderr, "Error: No MCP server named 'runsheet' found.\n")
				return sandbox.Exit{Code: 1}, nil
			}
			fmt.Fprintln(spec.Stdout, "runsheet")
		case len(cmd) > 0 && cmd[0] == "node":
			sc := bufio.NewScanner(spec.Stdin)
			for sc.Scan() {
				var m struct {
					ID     *int   `json:"id"`
					Method string `json:"method"`
				}
				if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil {
					continue
				}
				result := `{}`
				if m.Method == "tools/list" {
					result = `{"tools":[{"name":"mesh_search"}]}`
				}
				fmt.Fprintf(spec.Stdout, `{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", *m.ID, result)
			}
		case len(cmd) > 1 && cmd[0] == "codex" && cmd[1] == "sandbox":
			fmt.Fprintln(spec.Stdout, "127.0.0.1:40000")
		case len(cmd) > 1 && cmd[0] == "codex" && cmd[1] == "exec":
			_, _ = io.Copy(io.Discard, spec.Stdin)
			text, _ := json.Marshal(codexSays)
			fmt.Fprintf(spec.Stdout, `{"type":"thread.started","thread_id":"t1"}`+"\n"+
				`{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":%s}}`+"\n", text)
			// the verdict goes to the node's mesh_result (ADR-041), on the
			// stream as the mcp_tool_call Codex records
			if v := scriptedVerdict(codexSays); v != nil {
				args, _ := json.Marshal(map[string]any{"verdict": v})
				fmt.Fprintf(spec.Stdout, `{"type":"item.completed","item":{"id":"i2","type":"mcp_tool_call","server":"mesh-node","tool":"mesh_result","status":"completed","arguments":%s,"result":{"content":[{"type":"text","text":"recorded"}]}}}`+"\n", args)
			}
			fmt.Fprint(spec.Stdout, `{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}`+"\n")
		default:
			return sandbox.Exit{Code: 127}, nil
		}
		return sandbox.Exit{}, nil
	}}
}

// sandboxedCodexEvaluator is familyFixture's evaluator on a sandboxed
// node: the real Codex harness in the session's pod over rt, its
// Preflight the evaluator's, and runsheet the relay.
func sandboxedCodexEvaluator(t *testing.T, ev *Evaluator, rt *sandbox.Fake) {
	t.Helper()
	sb := &sandbox.Sandbox{Runtime: rt, TempDir: t.TempDir(), Config: sandbox.Config{Image: "ghcr.io/runsheet/mesh-node:v1"}}
	codex := &harness.Codex{Binary: "/nonexistent/codex", Sandbox: sb,
		Tokens: &TokenFiles{Token: func(context.Context) (string, error) { return "rs_test_bearer_0123456789", nil }}}
	ev.Sandbox = sb
	ev.Codex = ObserveLimits(codex, ev.Limits)
	ev.CodexPreflight = codex.Preflight
	ev.MCP = map[string]any{harness.BuiltinMCP: harness.MeshContextRelay("mesh", "https://api.test/mcp", "")}
}

func names(env []string) []string {
	var out []string
	for _, kv := range env {
		n, _, _ := strings.Cut(kv, "=")
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// A sandboxed Codex evaluation's preflight runs in the session's own pod
// before the session: the runtime records `codex mcp get runsheet`, the
// relay's tools/list handshake and the sandbox probe in the pod the
// session's `codex exec` then runs in, each with CODEX_HOME inside the
// session's HOME and the session's variables by name; the verdict is
// Codex's.
func TestSandboxedCodexEvaluationPreflightsInItsPod(t *testing.T) {
	f, ev, claude, _, _, logs := familyFixture(t, "normal", "sonnet", "VERDICT: FAIL", "Holds.\nVERDICT: PASS")
	rt := codexPodRuntime(false, "Holds.\nVERDICT: PASS")
	sandboxedCodexEvaluator(t, ev, rt)
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 0 || in["passed"] != true {
		t.Fatalf("claude ran %d, recorded %+v\n%s", len(claude.jobs), in, logs.String())
	}
	if u := usageOf(t, f); u["adapter"] != "codex" {
		t.Fatalf("usage %v", u)
	}
	if len(rt.Pods) != 1 {
		t.Fatalf("pods %d, want the one the preflight and the session share", len(rt.Pods))
	}
	pod := sandbox.Pod{Name: rt.Pods[0].Name}
	if len(rt.Agents) != 4 {
		t.Fatalf("containers %d: %+v", len(rt.Agents), rt.Agents)
	}
	exec := rt.Agents[3]
	if exec.Name != sandbox.AgentName(pod) || exec.Command[0] != "codex" || exec.Command[1] != "exec" {
		t.Fatalf("the session's container is %s %v", exec.Name, exec.Command)
	}
	codexHome := envOrEmpty(exec.Env, "CODEX_HOME")
	home := envOrEmpty(exec.Env, "HOME")
	if home == "" || !strings.HasPrefix(codexHome, home+"/") {
		t.Fatalf("the session's CODEX_HOME %s is not inside its HOME %s", codexHome, home)
	}
	for i, want := range [][]string{{"codex", "mcp", "get", harness.BuiltinMCP}, {"node"}, {"codex", "sandbox"}} {
		a := rt.Agents[i]
		if a.Name != sandbox.ExecName(pod, i+1) || !slices.Equal(a.Command[:len(want)], want) {
			t.Fatalf("container %d is %s %v, want a preflight step %v in pod %s", i, a.Name, a.Command, want, pod.Name)
		}
		if envOrEmpty(a.Env, "CODEX_HOME") != codexHome || envOrEmpty(a.Env, "HOME") != home {
			t.Fatalf("container %d's CODEX_HOME %s HOME %s, the session's %s %s", i, envOrEmpty(a.Env, "CODEX_HOME"), envOrEmpty(a.Env, "HOME"), codexHome, home)
		}
		if !slices.Equal(names(a.Env), names(exec.Env)) {
			t.Fatalf("container %d's env %v, the session's %v", i, names(a.Env), names(exec.Env))
		}
	}
}

// A sandboxed Codex preflight that fails in the pod moves an unpinned
// evaluation to Claude: the Claude harness runs, node.log names the
// preflight failure, and no `codex exec` ran.
func TestSandboxedCodexPreflightFailureEvaluatesOnClaude(t *testing.T) {
	f, ev, claude, _, _, logs := familyFixture(t, "normal", "sonnet", "Holds.\nVERDICT: PASS", "VERDICT: FAIL")
	rt := codexPodRuntime(true, "VERDICT: FAIL")
	sandboxedCodexEvaluator(t, ev, rt)
	in := evaluateItem1(t, f, ev)
	if len(claude.jobs) != 1 || in["passed"] != true {
		t.Fatalf("claude ran %d, recorded %+v", len(claude.jobs), in)
	}
	if claude.jobs[0].Session == nil {
		t.Error("the Claude session was not handed the evaluation's pod")
	}
	why := "codex mcp get runsheet: Error: No MCP server named 'runsheet' found."
	if want := "item ship-it/1: Codex preflight failed (" + why + "); evaluating on Claude"; !strings.Contains(logs.String(), want) {
		t.Fatalf("node.log lacks %q:\n%s", want, logs.String())
	}
	if notes, _ := in["notes"].(string); !strings.Contains(notes, "the Codex preflight failed: "+why) {
		t.Fatalf("notes %q", notes)
	}
	if u := usageOf(t, f); u["adapter"] != "claude-code" {
		t.Fatalf("usage %v", u)
	}
	for _, a := range rt.Agents {
		if slices.Contains(a.Command, "exec") {
			t.Fatalf("codex exec ran after a failed preflight: %v", a.Command)
		}
	}
	if len(rt.Agents) != 1 || rt.Agents[0].Command[1] != "mcp" {
		t.Fatalf("containers %+v", rt.Agents)
	}
}

func envOrEmpty(env []string, name string) string {
	v, _ := envOf(env, name)
	return v
}
