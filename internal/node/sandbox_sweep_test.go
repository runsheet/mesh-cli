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
	tmp := t.TempDir()
	rt := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: rt, TempDir: tmp}
	live, err := sb.Open("item 3", "build", sandbox.GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	stale := leftHome(t, tmp)
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
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("the stale HOME %s is still there", stale)
	}
	if _, err := os.Lstat(live.Home); err != nil {
		t.Fatalf("the running session's HOME went: %v", err)
	}
	if !slices.Equal(rt.Removed, []string{"mesh-stale-0"}) || !slices.Equal(rt.RemovedContainers, []string{"mesh-stale-0-agent"}) {
		t.Fatalf("removed pods %v, containers %v", rt.Removed, rt.RemovedContainers)
	}
}

// leftHome is a session HOME the process before this one left under
// tmp: made as Sandbox.Open makes one, by no session this process has
// open.
func leftHome(t *testing.T, tmp string) string {
	t.Helper()
	home, err := os.MkdirTemp(tmp, "mesh-home-")
	if err != nil {
		t.Fatal(err)
	}
	return home
}

// The sweep run on the quartermaster's cadence, while this process runs
// its sessions: a validation between two steps (its pod standing with
// only the infra container running, its HOME mounted by nothing) and a
// session between Open and its agent's first container (no pod yet) are
// both kept — pod, container and HOME — while a pod and HOME the
// process before left in the same state go. This is what killed 184
// validations on the dl360 on 2026-10-17 ("step exited 137", "no pod
// with ID") when the sweep first ran every ten minutes.
func TestCadenceSweepKeepsThisProcessesOpenSessions(t *testing.T) {
	tmp := t.TempDir()
	rt := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: rt, TempDir: tmp}
	validating, err := sb.WithStores(sandbox.Stores{}).Open("item 5", "validate", sandbox.GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	// the first step ran and exited; the second is not started yet
	if err := validating.Exec(context.Background(), sandbox.RunSpec{Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	pod := validating.Pods()[0].Name
	opening, err := sb.Open("item 6", "build", sandbox.GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	stale := leftHome(t, tmp)
	rt.Running = []sandbox.Container{
		{Name: pod + "-exec-1", Pod: pod},
		{Name: pod + "-infra", Pod: pod, Running: true, Infra: true},
		{Name: "mesh-stale-0-agent", Pod: "mesh-stale-0"},
		{Name: "mesh-stale-0-infra", Pod: "mesh-stale-0", Running: true, Infra: true},
	}
	rt.Standing = []sandbox.PodInfo{
		{Name: pod, Labels: map[string]string{sandbox.SessionLabel: "item 5"}},
		{Name: "mesh-stale-0", Labels: map[string]string{sandbox.SessionLabel: "item 2"}},
	}
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	res := SweepSandbox(context.Background(), sb, nil, logf)

	if want := (sandbox.SweepResult{Homes: 1, Pods: 1, Containers: 1, HomesInUse: 2}); res != want {
		t.Fatalf("swept %+v, want %+v (log %q)", res, want, lines)
	}
	if !slices.Equal(rt.Removed, []string{"mesh-stale-0"}) || !slices.Equal(rt.RemovedContainers, []string{"mesh-stale-0-agent"}) {
		t.Fatalf("removed pods %v, containers %v: the validation's went", rt.Removed, rt.RemovedContainers)
	}
	for _, home := range []string{validating.Home, opening.Home} {
		if _, err := os.Lstat(home); err != nil {
			t.Fatalf("an open session's HOME went: %v", err)
		}
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("the stale HOME %s is still there", stale)
	}

	// closed, the validation's pod and HOME are the sweep's to take
	if err := validating.Close(); err != nil {
		t.Fatal(err)
	}
	rt.Removed, rt.RemovedContainers = nil, nil
	rt.Running = []sandbox.Container{{Name: pod + "-infra", Pod: pod, Running: true, Infra: true}}
	rt.Standing = []sandbox.PodInfo{{Name: pod, Labels: map[string]string{sandbox.SessionLabel: "item 5"}}}
	if res := SweepSandbox(context.Background(), sb, nil, logf); res.Pods != 1 || res.HomesInUse != 1 {
		t.Fatalf("after Close swept %+v, want the closed session's pod gone and the opening session's HOME kept", res)
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
