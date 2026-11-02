package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// drainCalls are the node's POST /v1/nodes/{id}/drain bodies, in order.
func drainCalls(t *testing.T, g *rig) []meshapi.DrainRequest {
	t.Helper()
	var out []meshapi.DrainRequest
	for _, c := range g.fake.Calls("drain") {
		var d meshapi.DrainRequest
		if err := json.Unmarshal(c.Body, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// The operator's drain (ADR-044 amendment, drain): the node tells Mesh,
// whose answer to the take in its long poll (a scheduler's free slot
// left it waiting) is then 204, takes nothing else that is on offer,
// lets the running lease finish as it would, and Run ends nil.
func TestDrainFinishesTheRunningLeaseTakesNothingNewAndEnds(t *testing.T) {
	sess := &stubSession{calls: []string{"conform/submit"}, hold: make(chan struct{}), result: SessionResult{SessionID: "sess-1", Text: "done"}}
	g, _ := actorRig(t, sess)
	g.rt.Once = false
	g.rt.HeartbeatEvery = time.Hour
	// two slots: with one lease running the loop is in a long poll
	g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 2})
	g.rt.Node.CapacityTotal.Slots = 2
	g.fake.TakeWait = time.Minute
	var mu sync.Mutex
	var statuses []Status
	g.rt.OnStatus = func(s Status) { mu.Lock(); statuses = append(statuses, s); mu.Unlock() }
	job := g.offer(t, nil)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	waitFor(t, "the session", func() bool { return len(sess.specsSoFar()) == 1 })
	// the loop is back in a take, waiting for the next job
	waitFor(t, "the second take", func() bool { return len(g.fake.Calls("take")) >= 2 })

	g.rt.Drain("the test drains", 0)
	waitFor(t, "Mesh told", func() bool { return g.fake.Draining() })
	if d := drainCalls(t, g); len(d) != 1 || !d[0].Drain || d[0].Reason != "the test drains" {
		t.Fatalf("drain calls %+v", d)
	}
	if _, err := os.Stat(filepath.Join(g.work, DrainedMarker)); err != nil {
		t.Fatalf("no drain marker under the work root: %v", err)
	}
	st := g.rt.Status()
	if !st.Draining || len(st.Leases) != 1 || st.Leases[0].JobID != job {
		t.Fatalf("status while draining %+v", st)
	}
	// a job offered now is never taken
	takes := len(g.fake.Calls("take"))
	other := g.offer(t, nil)
	time.Sleep(200 * time.Millisecond)
	if n := len(g.fake.Calls("take")); n != takes {
		t.Fatalf("the draining node took again: %d takes, then %d", takes, n)
	}
	select {
	case err := <-done:
		t.Fatalf("Run ended while its lease ran: %v", err)
	default:
	}
	close(sess.hold)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil after a drain", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the drained node never ended")
	}
	if n := len(g.fake.Calls("finish")); n != 1 {
		t.Fatalf("%d finishes, want the running lease's one", n)
	}
	if n := len(g.fake.Calls("hand-back")); n != 0 {
		t.Fatalf("%d hand-backs after a drain with no wait", n)
	}
	if g.fake.LeaseOf(other) != "" {
		t.Fatal("the job offered after the drain was leased")
	}
	assertReleased(t, g, job)
	mu.Lock()
	defer mu.Unlock()
	if len(statuses) == 0 || !statuses[len(statuses)-1].Draining || len(statuses[len(statuses)-1].Leases) != 0 {
		t.Fatalf("last status %+v", statuses)
	}
}

// A drain with a wait hands back node_update — the job keeps its place —
// whatever still runs when the wait is up, and Run ends nil.
func TestDrainWaitHandsBackNodeUpdate(t *testing.T) {
	sess := &stubSession{hold: make(chan struct{})}
	g, _ := actorRig(t, sess)
	g.rt.Once = false
	g.rt.HeartbeatEvery = time.Hour
	g.rt.DrainTick = 10 * time.Millisecond
	job := g.offer(t, nil)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	waitFor(t, "the session", func() bool { return len(sess.specsSoFar()) == 1 })
	g.rt.Drain("the test drains", 200*time.Millisecond)
	if st := g.rt.Status(); st.HandBackAt == nil {
		t.Fatalf("status has no hand-back time: %+v", st)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait never handed the lease back")
	}
	hb := onlyHandBack(t, g, job)
	if hb.Cause != meshapi.CauseNodeUpdate || !strings.Contains(hb.Reason, "wait was up") {
		t.Fatalf("hand-back %+v", hb)
	}
	assertReleased(t, g, job)
}

// The node's next start finds the drain it asked Mesh for and resumes
// it before it takes or beats, so the restarted node takes again; a
// drain Mesh holds that the node did not ask for stays.
func TestTheNextStartResumesTheNodesOwnDrain(t *testing.T) {
	g := newRig(t, &stubSession{result: SessionResult{SessionID: "s", Text: "done"}})
	g.rt.HeartbeatEvery = 20 * time.Millisecond
	g.fake.Drain() // as the drained node left Mesh
	if err := os.WriteFile(filepath.Join(g.work, DrainedMarker), []byte("node\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := drainCalls(t, g); len(d) != 1 || d[0].Drain {
		t.Fatalf("drain calls %+v, want one resume", d)
	}
	calls := g.fake.Calls("")
	var order []string
	for _, c := range calls {
		if c.Route == "drain" || c.Route == "take" || c.Route == "heartbeat" {
			order = append(order, c.Route)
		}
	}
	if len(order) == 0 || order[0] != "drain" {
		t.Fatalf("the resume did not come first: %v", order)
	}
	if g.fake.LeaseOf(job) == "" {
		t.Fatal("the resumed node took nothing")
	}
	if _, err := os.Stat(filepath.Join(g.work, DrainedMarker)); !os.IsNotExist(err) {
		t.Fatalf("the marker outlived the resume: %v", err)
	}
}

// With no marker, a drain Mesh holds is not the node's to clear.
func TestADrainTheNodeDidNotAskForStays(t *testing.T) {
	g := newRig(t, &stubSession{})
	g.rt.Once = false
	g.rt.HeartbeatEvery = 20 * time.Millisecond
	g.fake.Drain()
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := drainCalls(t, g); len(d) != 0 {
		t.Fatalf("drain calls %+v", d)
	}
	if !g.fake.Draining() {
		t.Fatal("the node cleared a drain it did not ask for")
	}
}

// SIGTERM is not a drain: Run's context ending still hands the running
// lease back node_restart at once, drain or no drain.
func TestStopDuringADrainStillHandsBackNodeRestart(t *testing.T) {
	sess := &stubSession{hold: make(chan struct{})}
	g, _ := actorRig(t, sess)
	g.rt.Once = false
	g.rt.HeartbeatEvery = time.Hour
	job := g.offer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()
	waitFor(t, "the session", func() bool { return len(sess.specsSoFar()) == 1 })
	g.rt.Drain("the test drains", 0)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stopped node never ended")
	}
	if hb := onlyHandBack(t, g, job); hb.Cause != meshapi.CauseNodeRestart {
		t.Fatalf("hand-back %+v", hb)
	}
}

// The status names the node, its drain and its leases, for `mesh node
// status`.
func TestStatusDrainLines(t *testing.T) {
	now := time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)
	since, at := now.Add(-90*time.Second), now.Add(4*time.Minute)
	s := Status{PID: 42, NodeID: "n-1", Name: "dl360", Version: "v0.3.0", Draining: true, DrainReason: "`mesh node drain` asked",
		DrainSince: &since, HandBackAt: &at, Leases: []LeaseStatus{{LeaseID: "l-1", JobID: "j-1", Kind: "build", Since: now.Add(-time.Minute)}}}
	got := strings.Join(s.DrainLines(now), "\n")
	for _, want := range []string{"pid 42", "node n-1 (dl360)", "Draining for 1m30s", "in 4m0s is handed back node_update", "Leases: 1 running", "l-1  job j-1  build  for 1m0s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status lines\n%s\nlack %q", got, want)
		}
	}
	s.Draining, s.Leases = false, nil
	if got := strings.Join(s.DrainLines(now), "\n"); strings.Contains(got, "Draining") || !strings.Contains(got, "none running") {
		t.Fatalf("status lines\n%s", got)
	}
}
