//go:build !windows

package commands

import (
	"os"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/node"
)

// `mesh node drain` reads the pidfile and sends that process SIGUSR1:
// here the test's own, whose drain handler — the one `node start`
// installs — is called.
func TestNodeDrainSignalsThePidfilesProcess(t *testing.T) {
	state := node.RunState{Dir: t.TempDir()}
	pid := os.Getpid()
	if err := state.Started(pid); err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{}, 1)
	stop := onDrainSignal(func() {
		if err := state.MarkDraining(); err != nil {
			t.Error(err)
		}
		drained <- struct{}{}
	})
	defer stop()
	var out strings.Builder
	if err := drainNode(state, &out); err != nil {
		t.Fatal(err)
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGUSR1 never reached the drain handler")
	}
	if !strings.Contains(out.String(), "draining") {
		t.Fatalf("drain printed %q", out.String())
	}
	line, draining := localNodeState(state)
	if !draining || !strings.Contains(line, "draining") {
		t.Fatalf("status line %q, draining %v", line, draining)
	}
	state.Stopped(pid)
	if line, draining := localNodeState(state); draining || !strings.Contains(line, "no node running") {
		t.Fatalf("after exit: %q, draining %v", line, draining)
	}
}

// With no node running, drain says so instead of signalling anything.
func TestNodeDrainWithNoNode(t *testing.T) {
	state := node.RunState{Dir: t.TempDir()}
	if err := drainNode(state, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "no node is running") {
		t.Fatalf("drain with no pidfile: %v", err)
	}
	// a pidfile left by a process that is gone
	if err := state.Started(1 << 22); err != nil {
		t.Fatal(err)
	}
	if err := drainNode(state, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("drain with a stale pidfile: %v", err)
	}
}

// A running node with no marker is shown running, not draining.
func TestNodeStatusLineWithoutMarker(t *testing.T) {
	state := node.RunState{Dir: t.TempDir()}
	pid := os.Getpid()
	if err := state.Started(pid); err != nil {
		t.Fatal(err)
	}
	if line, draining := localNodeState(state); draining || strings.Contains(line, "draining") || !strings.Contains(line, "node running") {
		t.Fatalf("status line %q, draining %v", line, draining)
	}
}

// A running ranged node's capacity in force is what status shows for
// it, in-force/min–max (reason); a fixed one, or none running, shows
// the platform's capacity as before.
func TestNodeStatusShowsScaling(t *testing.T) {
	state := node.RunState{Dir: t.TempDir()}
	pid := os.Getpid()
	if err := state.Started(pid); err != nil {
		t.Fatal(err)
	}
	if _, ok := localScaling(state); ok {
		t.Fatal("a node that has not ticked shows scaling")
	}
	if err := state.WriteScaling(node.Scaling{Capacity: 2, Min: 1, Max: 4, Reason: node.ReasonWeeklyPace}, true); err != nil {
		t.Fatal(err)
	}
	if s, ok := localScaling(state); !ok || s.String() != "2/1–4 (weekly pace)" {
		t.Fatalf("scaling %q, %v", s, ok)
	}
	if err := state.WriteScaling(node.Scaling{}, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := localScaling(state); ok {
		t.Fatal("a fixed node shows scaling")
	}
	if err := state.WriteScaling(node.Scaling{Capacity: 1, Min: 1, Max: 2}, true); err != nil {
		t.Fatal(err)
	}
	state.Stopped(pid)
	if _, ok := localScaling(state); ok {
		t.Fatal("a stopped node shows scaling")
	}
}
