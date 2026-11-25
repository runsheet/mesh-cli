package nodev1

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
	"projectmesh.io/mesh-cli/internal/validate"
)

// logRuntime is testRuntime whose node.log lines are kept.
func logRuntime(t *testing.T, total scheduler.Capacity) (*Runtime, func() string) {
	t.Helper()
	rt := testRuntime(t)
	rt.Pods.Config.Limits = sandbox.Limits{CPUs: 8, Memory: "16g"}
	rt.Scheduler = scheduler.New(total)
	var mu sync.Mutex
	var logged []string
	rt.Logf = func(format string, args ...any) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	return rt, func() string { mu.Lock(); defer mu.Unlock(); return strings.Join(logged, "\n") }
}

// Goal a-validation-needing-no-room-is-admitted-at-once, acceptance
// check 3: with an older validation waiting for its lanes, a lint whose
// one lane fits its lease's own hold is admitted at once, and node.log
// says it went ahead and never that it waits.
func TestALintNeedingNoRoomIsAdmittedAheadOfAWaiter(t *testing.T) {
	rt, said := logRuntime(t, scheduler.Capacity{CPUs: 88, MemGB: 400})
	// the node holds 86 of its 88 cpus: 78 running, and two leases'
	// own holds of 4
	if _, err := rt.Scheduler.TryTake(scheduler.Ask{Owner: "running", CPUs: 78}); err != nil {
		t.Fatal(err)
	}
	big := validationBrief("big", "candidate", "runsheet/runsheet")
	lint := validationBrief("lint", "pull_request", "projectmesh-io/mesh-specs")
	for _, b := range []*meshapi.Brief{big, lint} {
		if _, err := rt.holdCapacity(context.Background(), b, leaseOf(b.LeaseID)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rt.admit(ctx, big, lanesOf("checks", 1, 16, "32g"), meshapi.Resources{})
	waitFor(t, "the big validation to wait", func() bool { return len(rt.Scheduler.Waiting()) == 1 })

	start := time.Now()
	lc, lcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer lcancel()
	got, err := rt.admit(lc, lint, lanesOf("markdown", 1, 4, "8g"), meshapi.Resources{})
	if err != nil || len(got) != 1 {
		t.Fatalf("the lint was not admitted: %v", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("the lint waited %s", waited)
	}
	log := said()
	if !strings.Contains(log, "lease lint: admitted ahead of 1 waiting: needs no extra room") {
		t.Fatalf("node.log does not say the lint went ahead:\n%s", log)
	}
	if strings.Contains(log, "lease lint: admission waits") {
		t.Fatalf("node.log says the lint waited:\n%s", log)
	}
}

// dl360, 2026-11-25 18:50–21:45 AEDT, reproduced: eight validations
// fill the node with their leases' own holds, then each waits for two
// lanes. Every own hold blocked every lane, so none was ever admitted;
// now the oldest is admitted from the holds the waiting leases yield.
func TestWaitingValidationsDoNotDeadlockTheNode(t *testing.T) {
	rt, said := logRuntime(t, scheduler.Capacity{CPUs: 32, MemGB: 128})
	briefs := make([]*meshapi.Brief, 8)
	for i := range briefs {
		b := validationBrief(fmt.Sprintf("v%d", i), "item", "runsheet/runsheet")
		if _, err := rt.holdCapacity(context.Background(), b, leaseOf(b.LeaseID)); err != nil {
			t.Fatalf("validation %d's own hold: %v", i, err)
		}
		briefs[i] = b
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	admitted := make(chan string, len(briefs))
	for _, b := range briefs {
		go func(b *meshapi.Brief) {
			if _, err := rt.admit(ctx, b, []validate.LaneSize{
				{Name: "unit", Resources: validate.Resources{CPUs: 8, Memory: "16g"}},
				{Name: "integration", Resources: validate.Resources{CPUs: 8, Memory: "16g"}},
			}, meshapi.Resources{}); err == nil {
				admitted <- b.LeaseID
			}
		}(b)
	}
	select {
	case id := <-admitted:
		t.Logf("%s admitted", id)
	case <-ctx.Done():
		t.Fatalf("no validation was admitted: the waiting leases' own holds blocked every lane (deadlock)\n%s", said())
	}
}

// The validation reserve is named to Mesh only while it has room for a
// whole admission like the largest it has held — the lease's own hold
// and its lanes — not for the own hold alone (dl360, 2026-11-25: Mesh
// placed validations into it four cpus at a time while their 16-cpu
// lanes could never fit).
func TestTheReserveIsNamedOnlyWithRoomForAWholeAdmission(t *testing.T) {
	rt := reserveRuntime(t, scheduler.Capacity{})
	b := validationBrief("v", "item", "runsheet/runsheet")
	own, err := rt.holdCapacity(context.Background(), b, leaseOf("v"))
	if err != nil {
		t.Fatal(err)
	}
	lanes, err := rt.admit(context.Background(), b, lanesOf("checks", 2, 8, "16g"), meshapi.Resources{})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lanes {
		l.l.Release()
	}
	own.Release()
	// 24 of the reserve's 40 cpus held elsewhere: 16 left, room for a
	// whole admission of 16 (4 own, 12 for lanes)
	filler, err := rt.Scheduler.TryTake(scheduler.Ask{Owner: "filler", CPUs: 24, Reserve: ValidationReserveName})
	if err != nil {
		t.Fatal(err)
	}
	tr := meshapi.TakeRequest{Free: rt.free()}
	rt.takeReserved(&tr, false)
	if tr.Reserved == nil {
		t.Fatal("the reserve with room for a whole admission is not named")
	}
	filler.Release()
	if _, err := rt.Scheduler.TryTake(scheduler.Ask{Owner: "filler", CPUs: 32, Reserve: ValidationReserveName}); err != nil {
		t.Fatal(err)
	}
	// 8 left: room for a lease's own hold, not for its lanes
	tr = meshapi.TakeRequest{Free: rt.free()}
	rt.takeReserved(&tr, false)
	if tr.Reserved != nil {
		t.Fatalf("the reserve is named with %g cpus left, under the 16 an admission takes", tr.Reserved.CPUs)
	}
}
