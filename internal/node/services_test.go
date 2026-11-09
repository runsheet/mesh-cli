package node

import (
	"context"
	"errors"
	"fmt"
	"io"

	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// servicesYML is a repository's .mesh/services.yml: Postgres required,
// Redis not, each with a health check the Fake answers.
const servicesYML = `services:
  postgres:
    image: postgres:16
    env:
      POSTGRES_PASSWORD: postgres
    ports: ["5432:5432"]
    options: --health-cmd "pg_isready -U postgres" --health-interval 10ms --health-retries 3
  redis:
    image: redis:7
    ports: ["6379"]
    required: false
`

// agentSeen is what the Fake's agent container saw: its prompt, its
// $HOME/.mesh-services and its env.
type agentSeen struct {
	mu       sync.Mutex
	prompt   string
	services string
	ran      bool
}

func writeServices(t *testing.T, dir, yml string) {
	t.Helper()
	if yml == "" {
		return
	}
	if err := os.MkdirAll(filepath.Join(dir, ".mesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sandbox.ServicesFile), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeAgent makes rt's agent container read its prompt and services
// file, then end as exit says.
func fakeAgent(rt *sandbox.Fake, exit func(ctx context.Context) (sandbox.Exit, error)) *agentSeen {
	seen := &agentSeen{}
	rt.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		prompt, _ := io.ReadAll(spec.Stdin)
		home, _ := envOf(spec.Env, "HOME")
		svc, _ := os.ReadFile(filepath.Join(home, sandbox.ServicesFileName))
		seen.mu.Lock()
		seen.prompt, seen.services, seen.ran = string(prompt), string(svc), true
		seen.mu.Unlock()
		if exit != nil {
			return exit(ctx)
		}
		fmt.Fprintln(spec.Stdout, `{"type":"system","subtype":"init","session_id":"s-1"}`)
		fmt.Fprintln(spec.Stdout, `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s-1","total_cost_usd":0.5}`)
		return sandbox.Exit{}, nil
	}
	return seen
}

func callIndex(t *testing.T, rt *sandbox.Fake, prefix string) int {
	t.Helper()
	i, ok := rt.Called(prefix)
	if !ok {
		t.Fatalf("no %q call: %v", prefix, rt.Calls)
	}
	return i
}

func notes(f *gatewaytest.Fake) string {
	var out []string
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		out = append(out, fmt.Sprint(c.Input["note"]))
	}
	return strings.Join(out, "\n")
}

// assertPodsGone: every pod made was removed with its sidecars, and no
// session HOME is left.
func assertPodsGone(t *testing.T, rt *sandbox.Fake, sb *sandbox.Sandbox) {
	t.Helper()
	if len(rt.Pods) != 1 || len(rt.Sidecars) != 2 {
		t.Fatalf("pods %d, sidecars %d: %v", len(rt.Pods), len(rt.Sidecars), rt.Calls)
	}
	if !slices.Contains(rt.Removed, rt.Pods[0].Name) {
		t.Fatalf("pod %s not removed: %v", rt.Pods[0].Name, rt.Calls)
	}
	left, _ := os.ReadDir(sb.TempDir)
	if len(left) != 0 {
		t.Fatalf("session HOMEs left: %v", left)
	}
}

// Every sandboxed session logs, before its services and its agent, the
// image ref its agent runs: `sandbox: session <label> — image <ref>`.
func TestOpenSessionPodLogsTheImage(t *testing.T) {
	const ref = "ghcr.io/runsheet/mesh-node:v0.2.335"
	rt := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v0.2.335"), TempDir: t.TempDir()}
	sb.Config.Image = ref
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	pod, err := OpenSessionPod(context.Background(), sb, t.TempDir(), "ship-it#1", harness.KindBuild, sandbox.GitIdentity{}, logf)
	if err != nil {
		t.Fatal(err)
	}
	defer pod.Close()
	want := "sandbox: session ship-it#1 — image " + ref
	var found int
	for _, l := range lines {
		if strings.Contains(l, "ship-it#1") && strings.Contains(l, ref) {
			found++
			if l != want {
				t.Fatalf("line = %q, want %q", l, want)
			}
		}
	}
	if found != 1 {
		t.Fatalf("lines naming the session and its image: %d, want 1: %q", found, lines)
	}
}

// A Close that cannot remove what the session left is logged through
// the node's logf, `sandbox: session <label>: close: <err>`, never dropped.
func TestSessionPodCloseLogsAFailure(t *testing.T) {
	rt := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{}.Settings("v1"), TempDir: t.TempDir()}
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	pod, err := OpenSessionPod(context.Background(), sb, t.TempDir(), "ship-it#1", harness.KindBuild, sandbox.GitIdentity{}, logf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pod.Session.Start(context.Background(), nil, 0); err != nil {
		t.Fatal(err)
	}
	rt.RemoveErr = errors.New("podman pod: pod is busy")
	lines = nil
	pod.Close()
	if len(lines) != 1 || lines[0] != "sandbox: session ship-it#1: close: podman pod: pod is busy" {
		t.Fatalf("lines = %q", lines)
	}
}
