//go:build !windows

package commands

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/meshapi/meshapitest"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/nodev1"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// stubUpgradeSession is a minimal nodev1.Session: it blocks on hold
// (closed to let the lease finish) and then answers result.
type stubUpgradeSession struct {
	hold   chan struct{}
	result nodev1.SessionResult

	mu     sync.Mutex
	starts int
}

func (s *stubUpgradeSession) Run(ctx context.Context, spec nodev1.SessionSpec) (nodev1.SessionResult, error) {
	s.mu.Lock()
	s.starts++
	s.mu.Unlock()
	select {
	case <-s.hold:
	case <-ctx.Done():
		return nodev1.SessionResult{}, ctx.Err()
	}
	return s.result, nil
}

func (s *stubUpgradeSession) started() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

// TestMeshNodeUpgradeV1SignalLeavesLeaseRunningToFinish is `mesh node
// upgrade --api v1` end to end against a real v1 node process: a real
// pidfile (the test's own pid, node.RunState.Started as `mesh node run
// --api v1` leaves it), the real restart handler noderun.go registers
// (onRestartSignal checking node.upgrade-ask before calling
// rt.UpgradeSignal, wired by hand here since nodeRunCmd.RunE builds its
// own nodev1.ClaudeSession with no seam for a fake one, but reading the
// same marker the real handler does), and v1Service{}.Restart -- the
// same SIGHUP `mesh node upgrade --api v1` sends, marked
// node.upgrade-ask first so it is told apart from `mesh node set`'s
// reload of the same signal (adr-044's amendment) -- run against that
// pid, never rt.UpgradeSignal called directly. The lease in flight is
// left running (adr-044's hand-off): no finish or hand-back is
// recorded until the held session is let go, at which point Mesh gets
// exactly one finish and no hand-back for the job.
func TestMeshNodeUpgradeV1SignalLeavesLeaseRunningToFinish(t *testing.T) {
	fake := meshapitest.NewFake(t)
	mesh := httptest.NewServer(fake)
	t.Cleanup(mesh.Close)

	_, nodeKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	sess := &stubUpgradeSession{hold: make(chan struct{}), result: nodev1.SessionResult{SessionID: "sess-1", Text: "done"}}
	rt := &nodev1.Runtime{
		SigningKey: nodeKey,
		API:        &meshapi.Client{BaseURL: mesh.URL, Token: func(context.Context) (string, error) { return "machine", nil }},
		Node: meshapi.RegisterRequest{
			Name: "test-node", Version: "v0.0.0-test",
			CapacityTotal: meshapi.Capacity{CPUs: 4, MemGB: 8, Slots: 1},
			SigningKey:    "ed25519:AAAA",
		},
		WorkRoot:      t.TempDir(),
		MeshBinary:    "/usr/local/bin/mesh",
		Session:       sess,
		Once:          true,
		Logf:          t.Logf,
		Backoff:       10 * time.Millisecond,
		Actors:        true,
		Sandbox:       &sandbox.Fake{},
		PodWatchEvery: 10 * time.Millisecond,
		Scheduler:     scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 1}),
	}

	// a minimal brief: no git checkout, no product/model servers, no
	// finish tool -- settle() synthesizes {"text": sess.result.Text} as
	// the finish with none of that, so the only thing under test is the
	// hand-off itself (lease.go's work(), settle.go).
	job := fake.Offer(func(b *meshapi.Brief) {
		b.Git = nil
		b.Servers = nil
		b.CredentialRef = ""
		b.Access = nil
		b.FinishTool = ""
		b.FinishSchema = nil
	})

	state := node.RunState{Dir: t.TempDir()}
	pid := os.Getpid()
	if err := v1Started(state, pid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v1Stopped(state, pid) })

	// UpgradeSignal (nodev1/upgrade_signal.go) calls UpgradeExit once
	// every live lease's hand-off is written -- the observable proof,
	// without ending this test binary, that the SIGHUP the Restart call
	// below sends actually reached rt.UpgradeSignal through the real
	// restart handler, not a direct call to it.
	var mu sync.Mutex
	var exits []int
	origExit := nodev1.UpgradeExit
	nodev1.UpgradeExit = func(code int) { mu.Lock(); exits = append(exits, code); mu.Unlock() }
	defer func() { nodev1.UpgradeExit = origExit }()
	exitCount := func() int { mu.Lock(); defer mu.Unlock(); return len(exits) }

	// mirrors noderun.go's own restart handler: SIGHUP alone does not
	// say which command meant it (`mesh node set`'s reload sends it
	// too, adr-044's amendment), so the handler reads and clears
	// node.upgrade-ask -- written by v1Service.Restart below, before it
	// signals -- to tell this SIGHUP was the upgrade's.
	stop := onRestartSignal(func() {
		if takeV1UpgradeAsk(state) {
			rt.UpgradeSignal("`mesh node upgrade` asked")
		}
	})
	defer stop()

	done := make(chan error, 1)
	go func() { done <- rt.Run(context.Background()) }()

	deadline := time.Now().Add(10 * time.Second)
	for sess.started() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := sess.started(); n != 1 {
		t.Fatalf("session starts %d, want exactly one before the upgrade", n)
	}
	if st := rt.Status(); len(st.Leases) != 1 || st.Leases[0].JobID != job {
		t.Fatalf("status before the upgrade %+v, want the one lease of job %s", st, job)
	}

	var out strings.Builder
	if err := (v1Service{}).Restart(context.Background(), state, &out); err != nil {
		t.Fatalf("v1Service.Restart: %v", err)
	}
	if !strings.Contains(out.String(), "upgrading") {
		t.Fatalf("Restart's message %q, want it to say the node is upgrading", out.String())
	}

	// Restart only sends the signal; the handler runs asynchronously.
	// Wait for UpgradeSignal to have written every lease's hand-off and
	// called UpgradeExit -- the proof that the real SIGHUP path (not a
	// direct call) carried the signal through.
	deadline = time.Now().Add(10 * time.Second)
	for exitCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	n, last := len(exits), append([]int(nil), exits...)
	mu.Unlock()
	if n != 1 || last[0] != 0 {
		t.Fatalf("UpgradeExit calls %v, want exactly one with code 0", last)
	}

	// the lease is left running, nothing yet reported to Mesh
	if n := len(fake.Calls("finish")) + len(fake.Calls("hand-back")); n != 0 {
		t.Fatalf("%d calls to Mesh right after the upgrade signal, want none yet: the upgrade should leave the lease running", n)
	}

	close(sess.hold)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil once the handed-off lease finishes", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lease outlived the upgrade signal")
	}

	if n := len(fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes, want the lease's one", n)
	}
	if n := len(fake.Calls("hand-back")); n != 0 {
		t.Fatalf("%d hand-backs, want none: the upgrade signal should have left the lease running to finish on its own", n)
	}
}
