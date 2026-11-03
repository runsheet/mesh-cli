package nodev1

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// With room at 48 CPUs fully taken, raising the capacity to 64 lets the
// next take that fits within it succeed at once — no restart — and
// node.log records the capacity change (ADR-044 amendment, 2026-11-03).
func TestCapacityRaisedUnblocksALeaseWithoutRestart(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	r := &Runtime{Logf: func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}}
	r.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 48})

	full, err := r.Scheduler.TryTake(scheduler.Ask{Owner: "a", CPUs: 48})
	if err != nil {
		t.Fatalf("taking all 48: %v", err)
	}
	defer full.Release()

	if _, err := r.Scheduler.TryTake(scheduler.Ask{Owner: "b", CPUs: 16}); !errors.Is(err, scheduler.ErrNoRoom) {
		t.Fatalf("a take with room fully taken: %v, want ErrNoRoom", err)
	}

	r.SetCapacity(meshapi.Capacity{CPUs: 64})

	second, err := r.Scheduler.TryTake(scheduler.Ask{Owner: "b", CPUs: 16})
	if err != nil {
		t.Fatalf("the next take within 64: %v, want it to succeed without a restart", err)
	}
	defer second.Release()

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, l := range logs {
		if strings.Contains(l, "capacity") && strings.Contains(l, "64") {
			found = true
		}
	}
	if !found {
		t.Fatalf("node.log did not record the capacity change: %v", logs)
	}
}

// With room at 64, 60 taken, cutting the capacity to 48 ends no running
// lease and takes nothing new until what is taken drops under 48
// (ADR-044 amendment, 2026-11-03).
func TestCapacityCutEndsNoLeaseAndWaitsForRoom(t *testing.T) {
	r := &Runtime{}
	r.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 64})

	held, err := r.Scheduler.TryTake(scheduler.Ask{Owner: "a", CPUs: 60})
	if err != nil {
		t.Fatalf("taking 60 of 64: %v", err)
	}

	r.SetCapacity(meshapi.Capacity{CPUs: 48})

	leases := r.Scheduler.Leases()
	if len(leases) != 1 || leases[0].Owner != "a" {
		t.Fatalf("leases after the cut: %+v, want the running lease untouched", leases)
	}

	if _, err := r.Scheduler.TryTake(scheduler.Ask{Owner: "b", CPUs: 1}); !errors.Is(err, scheduler.ErrNoRoom) {
		t.Fatalf("a take while 60 is held against a total of 48: %v, want ErrNoRoom", err)
	}

	held.Release()

	next, err := r.Scheduler.TryTake(scheduler.Ask{Owner: "c", CPUs: 40})
	if err != nil {
		t.Fatalf("a take once taken dropped under 48: %v", err)
	}
	defer next.Release()
}
