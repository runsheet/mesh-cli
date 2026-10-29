package nodev1

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// With a scheduler (PR 3.1) the node runs its leases side by side, a job
// slot each, reports what the live leases leave free on every take, and
// takes nothing while every slot is held.
func TestTheSchedulerOwnsTheNodesCapacity(t *testing.T) {
	sess := &stubSession{calls: []string{"conform/submit"}, hold: make(chan struct{})}
	g := newRig(t, sess)
	g.rt.Once = false
	g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 2})
	for range 3 {
		g.offer(t, func(b *meshapi.Brief) { b.Resources = meshapi.Resources{CPUs: 1.5, MemGB: 2} })
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()
	waitFor(t, "two sessions at once", func() bool { return len(sess.specsSoFar()) == 2 })
	time.Sleep(200 * time.Millisecond)
	takes := g.fake.Calls("take")
	if len(takes) != 2 {
		t.Fatalf("%d takes with both slots held, want 2", len(takes))
	}
	var first, second meshapi.TakeRequest
	json.Unmarshal(takes[0].Body, &first)
	json.Unmarshal(takes[1].Body, &second)
	if f := first.Free; f == nil || f.CPUs != 4 || f.MemGB != 8 || f.Slots != 2 {
		t.Fatalf("the first take's free %+v, want the whole node", f)
	}
	if f := second.Free; f == nil || f.CPUs != 2.5 || f.MemGB != 6 || f.Slots != 1 {
		t.Fatalf("the second take's free %+v, want the first lease's room taken", f)
	}
	if held := g.rt.Scheduler.Leases(); len(held) != 2 || !held[0].Ask.Slot || held[0].Ask.Cache != scheduler.CacheShared {
		t.Fatalf("the scheduler holds %+v", held)
	}
	close(sess.hold)
	waitFor(t, "the third lease", func() bool { return len(sess.specsSoFar()) == 3 })
	waitFor(t, "three finishes", func() bool { return len(g.fake.Calls("finish")) == 3 })
	waitFor(t, "every room back", func() bool { return len(g.rt.Scheduler.Leases()) == 0 })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime did not stop")
	}
}

// A lease Mesh gave the node that the scheduler has no room for (Mesh's
// ledger and the node's disagreeing) is handed back cannot_run, naming
// why, and spends no attempt.
func TestALeaseWithNoRoomHandsBack(t *testing.T) {
	g := newRig(t, &stubSession{calls: []string{"conform/submit"}})
	g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 4, Slots: 1})
	// a trim holds the build cache past the lease's wait
	trim, err := g.rt.Scheduler.TryTake(scheduler.Ask{Owner: "trim", Cache: scheduler.CacheExclusive})
	if err != nil {
		t.Fatal(err)
	}
	defer trim.Release()
	job := g.offer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	run(t, g, ctx)
	calls := g.fake.Calls("hand-back")
	if len(calls) != 1 || calls[0].LeaseID != g.fake.LeaseOf(job) {
		t.Fatalf("hand-backs %+v", calls)
	}
	var hb meshapi.HandBackRequest
	json.Unmarshal(calls[0].Body, &hb)
	if hb.Cause != meshapi.CauseCannotRun || hb.Reason == "" {
		t.Fatalf("hand-back %+v", hb)
	}
}
