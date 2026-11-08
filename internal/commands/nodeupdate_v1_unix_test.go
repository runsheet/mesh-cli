//go:build !windows

package commands

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/node"
)

// `mesh node update` drains a live Mesh v1 node beside the legacy one
// before it swaps the binary (nodeupdate.go, nodev1state.go): both run
// the one executable, so the v1 node's own pidfile under a test HOME's
// ~/.mesh/node-v1 is drained as drainIfRunning drains the legacy node's
// -- a real SIGUSR1 to the test's own pid, caught here rather than left
// to its default disposition -- and the command never says no node is
// running when the v1 one plainly is.
func TestNodeUpdateDrainsALiveV1Node(t *testing.T) {
	f := newUpdateFixture(t)
	f.u.Drain = func(node.RunState, io.Writer) (bool, error) { return false, nil } // no legacy node running

	home := t.TempDir()
	t.Setenv("HOME", home)
	v1, err := v1RunState()
	if err != nil {
		t.Fatal(err)
	}
	f.u.V1Run = v1
	pid := os.Getpid()
	if err := v1Started(v1, pid); err != nil {
		t.Fatal(err)
	}
	defer v1Stopped(v1, pid)

	drained := make(chan struct{}, 1)
	stop := onDrainSignal(func() { drained <- struct{}{} })
	defer stop()

	if err := f.u.run(context.Background(), updateTo, false); err != nil {
		t.Fatalf("update: %v\n%s", err, f.out)
	}

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the v1 node's SIGUSR1 never arrived")
	}

	if b, _ := os.ReadFile(f.exe); !bytes.Equal(b, f.bin) {
		t.Fatalf("executable = %q, want the release: the update should still swap once the v1 node is draining", b)
	}

	out := f.out.String()
	if strings.Contains(out, "No node is running") {
		t.Fatalf("output says no node is running despite a live v1 node:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("Mesh v1 node (pid %d) is draining", pid)) {
		t.Fatalf("output does not show the v1 node draining:\n%s", out)
	}
}

// With no v1 node running, the update's v1 drain is a no-op: the legacy
// drain's own "No node is running" wording (when neither is live) is
// unaffected by the v1 addition.
func TestNodeUpdateWithNoV1NodeDrainsNothingExtra(t *testing.T) {
	f := newUpdateFixture(t)
	f.u.Drain = func(node.RunState, io.Writer) (bool, error) { return false, nil } // no legacy node running
	f.u.V1Run = node.RunState{Dir: t.TempDir()}                                    // no v1 pidfile there

	if err := f.u.run(context.Background(), updateTo, false); err != nil {
		t.Fatalf("update: %v\n%s", err, f.out)
	}
	if b, _ := os.ReadFile(f.exe); !bytes.Equal(b, f.bin) {
		t.Fatalf("executable = %q, want the release", b)
	}
	if out := f.out.String(); !strings.Contains(out, "No node is running on this machine") {
		t.Fatalf("output does not say no node is running:\n%s", out)
	}
}
