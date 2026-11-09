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
	state.Stopped(pid)
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
