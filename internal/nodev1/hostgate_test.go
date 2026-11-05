package nodev1

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// fakeMachine is a MachineProbe a test can change mid-run, under its own
// lock, to move the host from hot (or short of memory) to quiet.
type fakeMachine struct {
	mu      sync.Mutex
	reading MachineReading
}

func (f *fakeMachine) set(r MachineReading) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reading = r
}

func (f *fakeMachine) Read() (MachineReading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reading, nil
}

// No lease is taken while the host reads hot; the next take goes through
// once it cools (Mesh v1 spec amendment, "A node backs off a hot host").
func TestHostGateHoldsBackATakeWhileHot(t *testing.T) {
	g := newRig(t, &stubSession{})
	g.rt.Host = Host{LoadPerCoreMax: 1.0}
	m := &fakeMachine{reading: MachineReading{Load1: 8, CPUs: 4}} // per-core 2.00, over 1.00
	g.rt.Machine = m
	job := g.offer(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := len(g.fake.Calls("take")); n != 0 {
			t.Fatalf("%d take call(s) while the host read hot", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	m.set(MachineReading{Load1: 1, CPUs: 4}) // per-core 0.25, under 1.00

	leaseDeadline := time.Now().Add(10 * time.Second)
	for g.fake.LeaseOf(job) == "" && time.Now().Before(leaseDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if g.fake.LeaseOf(job) == "" {
		t.Fatal("the lease was never taken once the host cooled")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// A lease already running completes untouched when the host turns hot
// mid-session; only the next take is held back.
func TestHostGateLeavesARunningLeaseUntouched(t *testing.T) {
	sess := &stubSession{calls: []string{"conform/submit"}, hold: make(chan struct{}), result: SessionResult{SessionID: "sess-ok"}}
	g := newRig(t, sess)
	g.rt.Host = Host{LoadPerCoreMax: 1.0}
	m := &fakeMachine{reading: MachineReading{Load1: 1, CPUs: 4}} // cool: per-core 0.25
	g.rt.Machine = m
	job := g.offer(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for len(sess.specsSoFar()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(sess.specsSoFar()) == 0 {
		t.Fatal("the session never started")
	}

	m.set(MachineReading{Load1: 8, CPUs: 4}) // now hot: per-core 2.00
	// long enough that the gate, if it wrongly touched a running lease,
	// would have acted
	time.Sleep(150 * time.Millisecond)
	close(sess.hold)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}

	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("the running lease ended %q, not finish: the host gate touched it while it ran (hand-backs %v)",
			g.fake.Ended(lease), g.fake.Calls("hand-back"))
	}
	fin := g.fake.Calls("finish")
	if len(fin) != 1 {
		t.Fatalf("%d finish calls", len(fin))
	}
	var req meshapi.FinishRequest
	json.Unmarshal(fin[0].Body, &req)
	if req.SessionRef != "sess-ok" {
		t.Fatalf("finish session_ref %q: the session was cut short, not let to finish on its own", req.SessionRef)
	}
}

// No new lease is taken while the host's free memory stays at or under
// Host.MinFreeMemory's floor.
func TestHostGateHoldsBackATakeWhileShortOfMemory(t *testing.T) {
	g := newRig(t, &stubSession{})
	g.rt.Host = Host{MinFreeMemory: 1 << 30} // 1 GiB floor
	m := &fakeMachine{reading: MachineReading{MemAvailable: 512 << 20}}
	g.rt.Machine = m
	job := g.offer(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := len(g.fake.Calls("take")); n != 0 {
			t.Fatalf("%d take call(s) while memory stayed at the floor", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	m.set(MachineReading{MemAvailable: 2 << 30}) // 2 GiB, above the floor

	leaseDeadline := time.Now().Add(10 * time.Second)
	for g.fake.LeaseOf(job) == "" && time.Now().Before(leaseDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if g.fake.LeaseOf(job) == "" {
		t.Fatal("the lease was never taken once memory recovered")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}
