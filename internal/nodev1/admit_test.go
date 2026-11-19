package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
	"projectmesh.io/mesh-cli/internal/validate"
)

// sevenLanes is a suite of seven 4-CPU, 8 GB lanes: 28 CPUs in all.
func sevenLanes() []validate.LaneSize {
	out := make([]validate.LaneSize, 7)
	for i := range out {
		out[i] = validate.LaneSize{Name: fmt.Sprintf("integration-%d", i), Resources: validate.Resources{CPUs: 4, Memory: "8g"}}
	}
	return out
}

// A validation is admitted for every lane it runs, all at once or not at
// all (goal a-validation-is-admitted-for-every-lane-it-runs): with 32
// CPUs free, a lease whose lanes sum to 28 CPUs is admitted and holds 28
// (its own 4 and 24 more for the lanes beside its pod); a second
// identical lease waits, holding nothing, until the first ends; a lease
// needing more than the node's total is refused naming why; and every
// admission logs asked, free and held.
func TestValidateLeaseAdmittedForAllLanes(t *testing.T) {
	rt := testRuntime(t)
	rt.Pods.Config.Limits = sandbox.Limits{CPUs: 8, Memory: "16g"}
	rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 32, MemGB: 128, Slots: 4})
	var mu sync.Mutex
	var logged []string
	rt.Logf = func(format string, args ...any) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	brief := func(id string) (*meshapi.Brief, *scheduler.Lease) {
		b := &meshapi.Brief{LeaseID: id, JobID: "job-" + id, Kind: "runsheet/validate", Resources: meshapi.Resources{CPUs: 4, MemGB: 8}}
		own, err := rt.Scheduler.TryTake(askOf(b, &meshapi.Lease{LeaseID: id, RenewEveryS: 30}))
		if err != nil {
			t.Fatalf("lease %s's own hold: %v", id, err)
		}
		return b, own
	}
	b1, own1 := brief("lease-1")
	first, err := rt.admit(context.Background(), b1, sevenLanes())
	if err != nil {
		t.Fatalf("the first lease was not admitted: %v", err)
	}
	if len(first) != 7 {
		t.Fatalf("%d lanes admitted, want 7", len(first))
	}
	if free := rt.Scheduler.Free(); free.CPUs != 4 {
		t.Fatalf("the admitted lease holds %g cpus, want 28", 32-free.CPUs)
	}
	for _, a := range first {
		if a.room.CPUs != 4 {
			t.Fatalf("lane %s's room is %g cpus, want 4", a.name, a.room.CPUs)
		}
	}

	// a second identical lease: its own hold fits, its lanes do not; it
	// waits holding none of them until the first lease ends
	rt.Scheduler.SetTotal(scheduler.Capacity{CPUs: 36, MemGB: 128, Slots: 4})
	b2, own2 := brief("lease-2")
	defer own2.Release()
	got := make(chan error, 1)
	var second []admittedLane
	go func() {
		var err error
		second, err = rt.admit(context.Background(), b2, sevenLanes())
		got <- err
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-got:
		t.Fatalf("the second lease did not wait: %v", err)
	default:
	}
	for _, h := range rt.Scheduler.Leases() {
		if strings.HasPrefix(h.Owner, "lease-2/") {
			t.Fatalf("the waiting lease holds %s: it must hold none of its lanes while it waits", h.Owner)
		}
	}
	for _, a := range first {
		a.l.Release()
	}
	own1.Release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("the second lease was not admitted once the first ended: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second lease was still waiting after the first ended")
	}
	if len(second) != 7 {
		t.Fatalf("%d lanes admitted for the second lease, want 7", len(second))
	}
	for _, a := range second {
		a.l.Release()
	}

	// a lease needing more than the node has in all is refused, naming why
	b3, own3 := brief("lease-3")
	defer own3.Release()
	big := append(sevenLanes(), sevenLanes()...)
	for i := range big {
		big[i].Name = fmt.Sprintf("big-%d", i)
	}
	if _, err := rt.admit(context.Background(), b3, big); !errors.Is(err, errNeverFits) || !strings.Contains(err.Error(), "the node has 36 in all") {
		t.Fatalf("a lease needing more than the node has was answered %v, want refused naming why", err)
	}

	mu.Lock()
	defer mu.Unlock()
	said := strings.Join(logged, "\n")
	for _, want := range []string{
		"lease lease-1: admitted 7 lanes: asked 24 cpus",
		"free 28 cpus",
		"held 28 cpus, 56 GB (the node holds 28 of its 32 cpus",
		"lease lease-2: admission waits: asked 24 cpus",
		"lease lease-3: admission refused",
	} {
		if !strings.Contains(said, want) {
			t.Fatalf("node.log lacks %q:\n%s", want, said)
		}
	}
}

// A lane's end gives its room back to its lease, not to the node: an
// admitted lease holds every lane's room until it ends.
func TestAnAdmittedLaneIsHeldUntilTheLeaseEnds(t *testing.T) {
	rt := testRuntime(t)
	rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 32, MemGB: 128})
	b := &meshapi.Brief{LeaseID: "lease-1", Kind: "validate", Resources: meshapi.Resources{CPUs: 4, MemGB: 8}}
	got, err := rt.admit(context.Background(), b, sevenLanes())
	if err != nil {
		t.Fatal(err)
	}
	p := &leasePod{}
	for _, a := range got {
		name := a.name
		h := wrapHold(a.l, a.room)
		h.back = func(h *laneHold) { p.setEagerLane(name, h) }
		p.setEagerLane(name, h)
	}
	h := p.popEagerLane("integration-3")
	h.release()
	if free := rt.Scheduler.Free(); free.CPUs != 8 {
		t.Fatalf("a lane's end gave its room to the node: %g cpus free, want 8", free.CPUs)
	}
	p.releaseEagerLanes()
	if free := rt.Scheduler.Free(); free.CPUs != 32 {
		t.Fatalf("the lease's end left %g cpus held", 32-free.CPUs)
	}
}

// The lanes a lease runs are in its hand-off (goal
// a-validation-is-admitted-for-every-lane-it-runs): a node restarted
// mid-lease reads them from handoff.json, adopts the lease's running
// lane pods — holding their room again and removing none of them while
// the lease runs — and removes them only at the lease's end.
func TestHandoffCarriesLanes(t *testing.T) {
	rt := testRuntime(t)
	fake := &sandbox.Fake{}
	rt.Sandbox = fake
	rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 32, MemGB: 128})
	id := "lease-1"
	rt.lanes.add(id, handoffLane{Name: "integration-1", Pod: "mesh-lease-1-lane-1", CPUs: 4, MemGB: 8})
	rt.lanes.add(id, handoffLane{Name: "integration-2", Pod: "mesh-lease-1-lane-2", CPUs: 4, MemGB: 8})
	b := &meshapi.Brief{LeaseID: id, Kind: "validate"}
	if err := rt.writeHandoff(id, &running{brief: b, sock: filepath.Join(t.TempDir(), "s.sock")}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(rt.leaseDir(id), handoffFile))
	if err != nil {
		t.Fatal(err)
	}
	var st handoffState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.Lanes == nil || len(st.Lanes.Pods) != 2 || st.Lanes.Pods[0].Pod != "mesh-lease-1-lane-1" {
		t.Fatalf("handoff.json carries lanes %+v, want both lane pods", st.Lanes)
	}

	// a new node: nothing held, no lanes known
	rt2 := testRuntime(t)
	rt2.WorkRoot = rt.WorkRoot
	rt2.Sandbox = fake
	rt2.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 32, MemGB: 128})
	a, err := actor.Start(rt2.leaseDir(id), actor.Record{LeaseID: id})
	if err != nil {
		t.Fatal(err)
	}
	rt2.adoptLanes(context.Background(), a, id, &st)
	if len(fake.Removed) != 0 {
		t.Fatalf("adopting the lease removed %v", fake.Removed)
	}
	if free := rt2.Scheduler.Free(); free.CPUs != 24 {
		t.Fatalf("the adopted lanes hold %g cpus, want 8", 32-free.CPUs)
	}
	if again := rt2.lanes.of(id); again == nil || len(again.Pods) != 2 {
		t.Fatalf("the adopting node does not carry the lanes into its own hand-off: %+v", again)
	}
	a.Release()
	if len(fake.Removed) != 2 {
		t.Fatalf("the lease's end removed %v, want both lane pods", fake.Removed)
	}
	if free := rt2.Scheduler.Free(); free.CPUs != 32 {
		t.Fatalf("the lease's end left %g cpus held", 32-free.CPUs)
	}
}
