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

// A node with no slot limit (--slots 0, ADR-044 amendment 2026-11-02)
// registers Slots 0, says nothing of slots on its takes, and runs as
// many leases side by side as its CPUs and memory hold.
func TestNoSlotLimitSchedulesOnCPUsAndMemory(t *testing.T) {
	sess := &stubSession{calls: []string{"conform/submit"}, hold: make(chan struct{})}
	g := newRig(t, sess)
	g.rt.Once = false
	g.rt.Node.CapacityTotal = meshapi.Capacity{CPUs: 8, MemGB: 16}
	g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 8, MemGB: 16})
	for range 3 {
		g.offer(t, func(b *meshapi.Brief) { b.Resources = meshapi.Resources{CPUs: 2, MemGB: 4} })
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()
	waitFor(t, "three sessions at once", func() bool { return len(sess.specsSoFar()) == 3 })
	var reg meshapi.RegisterRequest
	json.Unmarshal(g.fake.Calls("register")[0].Body, &reg)
	if reg.CapacityTotal.Slots != 0 || reg.CapacityTotal.CPUs != 8 {
		t.Fatalf("registered %+v, want no slot limit", reg.CapacityTotal)
	}
	takes := g.fake.Calls("take")
	var third meshapi.TakeRequest
	json.Unmarshal(takes[2].Body, &third)
	if f := third.Free; f == nil || f.CPUs != 4 || f.MemGB != 8 || f.Slots != 0 {
		t.Fatalf("the third take's free %+v, want two leases' room taken and no slots", f)
	}
	if held := g.rt.Scheduler.Leases(); len(held) != 3 {
		t.Fatalf("the scheduler holds %d leases, want 3", len(held))
	}
	close(sess.hold)
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
	// another holder has the node's CPUs past the lease's wait, and no
	// slot: a slot is free, so the node takes. (A trim holding the cache
	// no longer shows this: the node takes nothing during one,
	// TestANodeTakesNothingWhileATrimIsPending.)
	other, err := g.rt.Scheduler.TryTake(scheduler.Ask{Owner: "other", CPUs: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	job := g.offer(t, func(b *meshapi.Brief) { b.Resources = meshapi.Resources{CPUs: 2} })
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

// freeOf already excludes a lease's unopened lanes, as the take loop's
// own free() sees them on its next poll: the lease's own pod-level hold
// (holdCapacity, taken before openLeasePod in production) plus its 4
// eagerly-reserved lane holds (reserveLanes, ADR-049 amendment) are both
// out of the reported free room before any lane has run — 1 pod + 4
// lanes, 5 holds of 4 CPUs and 8 GB (the lease's allowance, and what
// the suite asks for each lane), not just the pod's one.
func TestFreeAlreadyExcludesAHeldLeasesUnopenedLanes(t *testing.T) {
	g := newShardRig(t, sizedYML, 400*time.Millisecond, scheduler.Capacity{CPUs: 100, MemGB: 100, Slots: 10})
	lease := &meshapi.Lease{LeaseID: g.b.LeaseID, JobID: g.b.JobID, N: g.b.LeaseN, RenewEveryS: g.b.RenewEveryS}
	pod, err := g.rt.holdCapacity(context.Background(), g.b, lease)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pod.Release)

	free := freeOf(g.rt.Scheduler)
	// admission (admitLanes, validate.AdmitSizes) takes one lane per
	// sharded suite, and the first lane runs in the lease's own pod on
	// the lease's own hold: only the pod is held until a run opens the
	// further shards
	wantCPUs, wantMem := 100-g.b.Resources.CPUs, 100-g.b.Resources.MemGB
	if free.CPUs != wantCPUs || free.MemGB != wantMem {
		t.Fatalf("free = %+v, want %g CPUs and %g MemGB free (the pod alone, its first lane within it, of %+v)", free, wantCPUs, wantMem, g.b.Resources)
	}
}

// Without a scheduler the node runs one lease at a time: its takes say
// one slot free even when its total has no slot limit.
func TestNoSchedulerTakesOneSlotWithNoSlotLimit(t *testing.T) {
	r := &Runtime{Node: meshapi.RegisterRequest{CapacityTotal: meshapi.Capacity{CPUs: 8, MemGB: 16}}}
	if f := r.free(); f.Slots != 1 || f.CPUs != 8 || f.MemGB != 16 {
		t.Fatalf("free %+v, want the whole node and one slot", f)
	}
	if r.Node.CapacityTotal.Slots != 0 {
		t.Fatal("free changed the node's total")
	}
}

// While a trim of the build cache is pending or running the node asks
// Mesh for nothing — it takes no lease, so none is handed back
// cannot_run for the trim — and the free capacity it would report is
// zero. The trim over, it takes the job that waited.
func TestANodeTakesNothingWhileATrimIsPending(t *testing.T) {
	g := newRig(t, &stubSession{calls: []string{"conform/submit"}})
	g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 4, MemGB: 8, Slots: 1})
	// a build holds the cache, so the trim waits for it: pending
	build, err := g.rt.Scheduler.TryTake(scheduler.Ask{Owner: "build", Cache: scheduler.CacheShared})
	if err != nil {
		t.Fatal(err)
	}
	trimCtx, stopTrim := context.WithCancel(context.Background())
	defer stopTrim()
	trimmed := make(chan struct{})
	go func() {
		defer close(trimmed)
		if l, err := g.rt.Scheduler.Take(trimCtx, scheduler.Ask{Owner: "trim", Cache: scheduler.CacheExclusive}); err == nil {
			l.Release()
		}
	}()
	waitFor(t, "the trim to wait", g.rt.Scheduler.Trimming)
	if f := g.rt.free(); f.CPUs != 0 || f.MemGB != 0 || f.Slots != 0 {
		t.Fatalf("free capacity reported during a pending trim: %+v, want zero", f)
	}
	job := g.offer(t, nil)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	time.Sleep(300 * time.Millisecond)
	if n := len(g.fake.Calls("take")); n != 0 {
		t.Fatalf("the node asked Mesh for work %d times during a pending trim", n)
	}
	if n := len(g.fake.Calls("hand-back")); n != 0 {
		t.Fatalf("%d leases handed back during a pending trim", n)
	}
	// the build ends, the trim runs and ends: the node takes the job
	build.Release()
	<-trimmed
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the node never took the job once the trim ended")
	}
	if n := len(g.fake.Calls("hand-back")); n != 0 {
		t.Fatalf("%d leases handed back", n)
	}
	if calls := g.fake.Calls("finish"); len(calls) != 1 || calls[0].LeaseID != g.fake.LeaseOf(job) {
		t.Fatalf("finishes %+v", calls)
	}
}
