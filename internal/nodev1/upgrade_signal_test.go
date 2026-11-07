package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// TestNodeUpgradeV1AdoptsRunningLease is UpgradeSignal end to end: the
// restart signal a `mesh node upgrade --api v1` sends a running v1 node
// (internal/commands/nodeupgrade.go, wired from
// internal/commands/noderun.go's onRestartSignal the way its drain
// signal calls Drain). The lease in flight is left running -- its
// hand-off written, nothing cancelled -- and UpgradeSignal exits the
// process (UpgradeExit, swapped here) rather than waiting on it; the
// lease goes on to finish on its own, so Mesh records exactly one
// finish and no hand-back for the job (adr-044, the upgrade
// amendment).
func TestNodeUpgradeV1AdoptsRunningLease(t *testing.T) {
	sess := &stubSession{calls: []string{"conform/submit"}, hold: make(chan struct{}), result: SessionResult{SessionID: "sess-1", Text: "done"}}
	g, _ := actorRig(t, sess)
	job := g.offer(t, nil)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	waitFor(t, "the session", func() bool { return len(sess.specsSoFar()) == 1 })

	if st := g.rt.Status(); len(st.Leases) != 1 || st.Leases[0].JobID != job {
		t.Fatalf("status before the upgrade signal %+v, want the one lease of job %s", st, job)
	}

	var mu sync.Mutex
	var exits []int
	orig := UpgradeExit
	UpgradeExit = func(code int) { mu.Lock(); exits = append(exits, code); mu.Unlock() }
	defer func() { UpgradeExit = orig }()

	g.rt.UpgradeSignal("the node is updating")

	mu.Lock()
	n, last := len(exits), exits
	mu.Unlock()
	if n != 1 || last[0] != 0 {
		t.Fatalf("UpgradeExit calls %v, want exactly one with code 0", last)
	}

	// the lease is left running, nothing yet reported to Mesh
	if n := len(g.fake.Calls("finish")) + len(g.fake.Calls("hand-back")); n != 0 {
		t.Fatalf("%d calls to Mesh right after the upgrade signal, want none yet", n)
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

	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes, want the lease's one", n)
	}
	if n := len(g.fake.Calls("hand-back")); n != 0 {
		t.Fatalf("%d hand-backs, want none: the upgrade should have left the lease running", n)
	}
}

// UpgradeSignal waits for a hand-back Upgrade makes to be reported to
// Mesh before it exits: a lease whose tunnel terminates TLS can never
// be picked back up by a hand-off, so it is handed back instead
// (handoff.go's Upgrade), and the restart signal must not exit the
// process before Mesh has heard of it -- an os.Exit cut off mid-report
// would leave the lease's actor.json Cancelling and unreported on disk
// forever, for no later process to ever adopt or release it (adr-044's
// hand-off adopts a hand-off, not a hand-back).
func TestUpgradeSignalWaitsForHandBackBeforeExit(t *testing.T) {
	sess := &stubSession{hold: make(chan struct{})}
	g, sb := actorRig(t, sess)
	g.rt.Pods = &sandbox.Sandbox{Runtime: sb, Logf: t.Logf, Config: sandbox.Config{
		Image:          "mesh-node:test",
		Limits:         sandbox.Limits{CPUs: 2, Memory: "6g", Pids: 4096},
		ServicesLimits: sandbox.Limits{CPUs: 2, Memory: "4g"},
		Registries:     sandbox.DefaultRegistries,
		ServiceTimeout: 5 * time.Second,
	}}
	registryHostRig(t, g)
	job := g.offer(t, registryBrief)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	waitFor(t, "the session", func() bool { return len(sess.specsSoFar()) == 1 })
	id := g.fake.LeaseOf(job)
	dir := g.rt.leaseDir(id)

	var mu sync.Mutex
	var reportedBeforeExit bool
	var leftCancellingOnDisk bool
	orig := UpgradeExit
	UpgradeExit = func(code int) {
		mu.Lock()
		defer mu.Unlock()
		reportedBeforeExit = len(g.fake.Calls("hand-back")) == 1
		if b, err := os.ReadFile(filepath.Join(dir, actor.FileName)); err == nil {
			var rec actor.Record
			if json.Unmarshal(b, &rec) == nil && (rec.State == actor.Cancelling || !rec.Reported) {
				leftCancellingOnDisk = true
			}
		}
		// a directory already gone is fully released, never Cancelling
	}
	defer func() { UpgradeExit = orig }()

	g.rt.UpgradeSignal("the node is updating")

	mu.Lock()
	reported, cancelling := reportedBeforeExit, leftCancellingOnDisk
	mu.Unlock()
	if !reported {
		t.Fatal("UpgradeExit ran before the hand-back it caused was reported to Mesh")
	}
	if cancelling {
		t.Fatal("UpgradeExit ran while the lease's actor.json was still Cancelling or unreported")
	}
	onlyHandBack(t, g, job)
	assertReleased(t, g, job)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not end")
	}
}
