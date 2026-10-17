package sandbox

import (
	"context"
	"testing"
)

// Exec runs in the session's pod without removing it: the pod it made is
// the one the next Run takes, each Exec its own container beside the
// agent's, with the agent's env and mounts.
func TestExecSharesThePodWithTheNextRun(t *testing.T) {
	f := &Fake{}
	sb := &Sandbox{Runtime: f, TempDir: t.TempDir(), Config: Config{Image: "img"}}
	s, err := sb.Open("g/7", "evaluate", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ws := t.TempDir()
	env := []string{"CODEX_HOME=/elsewhere", "MESH_ACCOUNT=a", "NOT_ALLOWED=1"}
	for range 2 {
		if err := s.Exec(context.Background(), RunSpec{Workspace: ws, Env: env, Command: []string{"codex", "mcp", "get", "runsheet"}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.Pods) != 1 || len(f.Removed) != 0 {
		t.Fatalf("pods %d removed %v", len(f.Pods), f.Removed)
	}
	if err := s.Run(context.Background(), RunSpec{Workspace: ws, Env: env, Command: []string{"codex", "exec"}}); err != nil {
		t.Fatal(err)
	}
	pod := Pod{Name: f.Pods[0].Name}
	if len(f.Pods) != 1 || len(f.Agents) != 3 || f.Agents[0].Name != ExecName(pod, 1) || f.Agents[1].Name != ExecName(pod, 2) || f.Agents[2].Name != AgentName(pod) {
		t.Fatalf("pods %d agents %+v", len(f.Pods), f.Agents)
	}
	for i, a := range f.Agents {
		if len(a.Env) != len(f.Agents[2].Env) || len(a.Mounts) != len(f.Agents[2].Mounts) {
			t.Fatalf("container %d env %v mounts %v, the agent's %v %v", i, a.Env, a.Mounts, f.Agents[2].Env, f.Agents[2].Mounts)
		}
		for j := range a.Env {
			if a.Env[j] != f.Agents[2].Env[j] {
				t.Fatalf("container %d env %v, the agent's %v", i, a.Env, f.Agents[2].Env)
			}
		}
	}
	if len(f.Removed) != 1 {
		t.Fatalf("removed %v: Run removes the shared pod at its end", f.Removed)
	}
}

// A non-zero exit of an Exec is an *ExitError.
func TestExecExitError(t *testing.T) {
	f := &Fake{Agent: func(context.Context, AgentSpec) (Exit, error) { return Exit{Code: 1}, nil }}
	sb := &Sandbox{Runtime: f, TempDir: t.TempDir(), Config: Config{Image: "img"}}
	s, err := sb.Open("g/7", "evaluate", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Exec(context.Background(), RunSpec{Workspace: t.TempDir(), Command: []string{"false"}})
	if ee, ok := err.(*ExitError); !ok || ee.Code != 1 {
		t.Fatalf("err = %v", err)
	}
}
