package nodev1

import (
	"context"
	"sync"
	"testing"
	"time"
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
