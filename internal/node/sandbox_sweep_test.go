package node

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// A node restart with adoption: the process before this one left two
// sandboxed sessions — one still running (its agent's container up,
// mounting its HOME), one whose agent exited with the old process. The
// start-up sweep removes the second's HOME, pod and container, keeps the
// first's, and logs the counts in one line.
func TestStartUpSweepLogsWhatItSwept(t *testing.T) {
	rt := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: rt, TempDir: t.TempDir()}
	live, err := sb.Open("item 3", "build", sandbox.GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := sb.Open("item 2", "build", sandbox.GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	rt.Running = []sandbox.Container{
		{Name: "mesh-live-0-agent", Pod: "mesh-live-0", Running: true, Mounts: []string{live.Home}},
		{Name: "mesh-live-0-infra", Pod: "mesh-live-0", Running: true, Infra: true},
		{Name: "mesh-stale-0-agent", Pod: "mesh-stale-0"},
		{Name: "mesh-stale-0-infra", Pod: "mesh-stale-0", Running: true, Infra: true},
	}
	rt.Standing = []sandbox.PodInfo{
		{Name: "mesh-live-0", Labels: map[string]string{sandbox.SessionLabel: "item 3"}},
		{Name: "mesh-stale-0", Labels: map[string]string{sandbox.SessionLabel: "item 2"}},
	}
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	SweepSandbox(context.Background(), sb, nil, logf)

	want := "sandbox: swept 1 homes, 1 pods, 1 containers at start (1 homes in use)"
	if !slices.Contains(lines, want) {
		t.Fatalf("log %q, want the line %q", lines, want)
	}
	if _, err := os.Lstat(stale.Home); !os.IsNotExist(err) {
		t.Fatalf("the stale HOME %s is still there", stale.Home)
	}
	if _, err := os.Lstat(live.Home); err != nil {
		t.Fatalf("the running session's HOME went: %v", err)
	}
	if !slices.Equal(rt.Removed, []string{"mesh-stale-0"}) || !slices.Equal(rt.RemovedContainers, []string{"mesh-stale-0-agent"}) {
		t.Fatalf("removed pods %v, containers %v", rt.Removed, rt.RemovedContainers)
	}
}

// No sandbox (`sandbox: off`) sweeps and logs nothing.
func TestStartUpSweepWithNoSandbox(t *testing.T) {
	var lines []string
	SweepSandbox(context.Background(), nil, nil, func(f string, a ...any) { lines = append(lines, f) })
	if len(lines) != 0 {
		t.Fatalf("logged %q", lines)
	}
}
