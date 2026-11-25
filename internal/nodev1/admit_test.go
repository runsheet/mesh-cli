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
		out[i] = validate.LaneSize{Name: fmt.Sprintf("integration-%d", i), Resources: validate.Resources{CPUs: 4, Memory: "8g"}, Services: true}
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
	first, err := rt.admit(context.Background(), b1, sevenLanes(), meshapi.Resources{})
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
		second, err = rt.admit(context.Background(), b2, sevenLanes(), meshapi.Resources{})
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
	if _, err := rt.admit(context.Background(), b3, big, meshapi.Resources{}); !errors.Is(err, errNeverFits) || !strings.Contains(err.Error(), "the node has 36 in all") {
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

// A lane's end gives its room back to the node, not to its lease: an
// admitted lease holds a lane's room only while a run_suite step runs
// in it, and a later step that needs the lane again (a re-run) takes
// the same room again, waiting for it as any lane does. The lease's own
// hold stays until it ends.
func TestAnAdmittedLaneGivesItsRoomBackWhenItEnds(t *testing.T) {
	cfg, err := validate.Parse([]byte(runsheetLike))
	if err != nil {
		t.Fatal(err)
	}
	rt := testRuntime(t)
	var mu sync.Mutex
	var logged []string
	rt.Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	rt.Pods.Config.Limits = sandbox.Limits{CPUs: 8, Memory: "16g"}
	rt.Pods.Config.ServicesLimits = sandbox.Limits{CPUs: 4, Memory: "8g"}
	rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 80, MemGB: 200, Slots: 8})
	b := &meshapi.Brief{LeaseID: "lease-r", JobID: "job-r", Kind: "runsheet/validate", Resources: meshapi.Resources{CPUs: 4, MemGB: 8}}
	own, err := rt.Scheduler.TryTake(askOf(b, &meshapi.Lease{LeaseID: "lease-r", RenewEveryS: 30}))
	if err != nil {
		t.Fatal(err)
	}
	p := &leasePod{statuses: []sandbox.ServiceStatus{{Name: "postgres", Ready: true}}}
	got, err := rt.admit(context.Background(), b, validate.AdmitSizes(cfg.Steps), rt.servicesRoom(p))
	if err != nil {
		t.Fatal(err)
	}
	rt.keepAdmitted(b, p, got)
	held := func() float64 { return 80 - rt.Scheduler.Free().CPUs }
	if h := held(); h != 16 {
		t.Fatalf("admitted, the lease holds %g cpus with its own, want 16", h)
	}
	sizes := map[string]validate.LaneSize{}
	for _, s := range validate.LaneSizes(cfg.Steps) {
		sizes[s.Name] = s
	}

	// the unit lane runs its steps on admission's hold and gives its 4
	// cpus back to the node as its last step exits
	unit, err := rt.holdLane(context.Background(), b, p, "unit", sizes["unit"], false)
	if err != nil {
		t.Fatal(err)
	}
	if h := held(); h != 16 {
		t.Fatalf("opening the admitted unit lane took more room: %g cpus held, want 16", h)
	}
	unit.release()
	if h := held(); h != 12 {
		t.Fatalf("after the unit lane ended the node holds %g cpus, want 12 (its 4 back to the node)", h)
	}

	// a re-run in the unit lane readmits its 4 cpus, and gives them back again
	again, err := rt.holdLane(context.Background(), b, p, "unit", sizes["unit"], false)
	if err != nil {
		t.Fatal(err)
	}
	if h := held(); h != 16 {
		t.Fatalf("a re-run in the unit lane holds %g cpus, want 16 (its 4 readmitted)", h)
	}
	again.release()
	if h := held(); h != 12 {
		t.Fatalf("after the re-run ended the node holds %g cpus, want 12", h)
	}

	// a re-run on a full node waits for the lane's room, and is
	// readmitted once room comes back
	fill, err := rt.Scheduler.TryTake(scheduler.Ask{Owner: "other", CPUs: rt.Scheduler.Free().CPUs, MemGB: 1})
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	var late *laneHold
	go func() {
		var err error
		late, err = rt.holdLane(context.Background(), b, p, "unit", sizes["unit"], false)
		waited <- err
	}()
	select {
	case err := <-waited:
		t.Fatalf("a re-run on a full node did not wait for room: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	fill.Release()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the re-run was still waiting once room came back")
	}
	late.release()

	// the integration lane, its Postgres with it: 8 back as it ends
	integ, err := rt.holdLane(context.Background(), b, p, "integration-0", sizes["integration-0"], false)
	if err != nil {
		t.Fatal(err)
	}
	integ.release()
	if h := held(); h != 4 {
		t.Fatalf("after every lane ended the node holds %g cpus, want 4 (the lease's own)", h)
	}
	p.releaseEagerLanes()
	if h := held(); h != 4 {
		t.Fatalf("the lease's own hold is %g cpus at its end, want 4 until the lease releases it", h)
	}
	own.Release()
	if h := held(); h != 0 {
		t.Fatalf("the lease's end left %g cpus held", h)
	}

	mu.Lock()
	defer mu.Unlock()
	said := strings.Join(logged, "\n")
	for _, want := range []string{
		"lease lease-r: lane unit ended: its 4 cpus, 8 GB back to the node",
		"lease lease-r: lane unit readmitted: held 4 cpus, 8 GB again",
		"lease lease-r: lane unit waits to be readmitted",
		"lease lease-r: lane integration-0 ended: its 8 cpus, 16 GB back to the node",
	} {
		if !strings.Contains(said, want) {
			t.Fatalf("node.log lacks %q:\n%s", want, said)
		}
	}
}

// A lease whose lanes ask for nothing beyond its own hold is admitted
// at once, even behind an older validation still waiting for its
// lanes' room: it draws no CPUs or memory the waiter is waiting for
// (goal a-validation-needing-no-room-is-admitted-at-once; dl360/
// mesh-specs#227).
func TestAdmitNeedingNoRoomJumpsTheQueue(t *testing.T) {
	rt := testRuntime(t)
	rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 20, MemGB: 100})
	var mu sync.Mutex
	var logged []string
	rt.Logf = func(format string, args ...any) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	if _, err := rt.Scheduler.TryTakeAll([]scheduler.Ask{{Owner: "running", CPUs: 16}}); err != nil {
		t.Fatal(err)
	}
	older := make(chan error, 1)
	go func() {
		_, err := rt.Scheduler.TakeAll(context.Background(), []scheduler.Ask{{Owner: "older", CPUs: 10}})
		older <- err
	}()
	// the older take is in line
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(rt.Scheduler.Waiting()) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the older take never waited")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// its one lane's room (4 cpus, 8 GB) fits inside its own lease's
	// hold (b.Resources): it asks the scheduler for nothing beyond that
	b := &meshapi.Brief{LeaseID: "lease-z", JobID: "job-z", Kind: "runsheet/validate", Resources: meshapi.Resources{CPUs: 4, MemGB: 8}}
	sizes := []validate.LaneSize{{Name: "checks", Resources: validate.Resources{CPUs: 4, Memory: "8g"}}}
	got, err := rt.admit(context.Background(), b, sizes, meshapi.Resources{})
	if err != nil {
		t.Fatalf("admit refused a lease needing no extra room: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d lanes admitted, want 1", len(got))
	}
	defer got[0].l.Release()

	select {
	case err := <-older:
		t.Fatalf("the older waiter was admitted instead of still waiting: %v", err)
	default:
	}

	mu.Lock()
	defer mu.Unlock()
	said := strings.Join(logged, "\n")
	if !strings.Contains(said, "lease lease-z: admitted ahead of 1 waiting: needs no extra room") {
		t.Fatalf("node.log lacks the zero-room admission line:\n%s", said)
	}
	if strings.Contains(said, "lease lease-z: admission waits") {
		t.Fatalf("node.log says lease-z waited, want it admitted at once:\n%s", said)
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

// Each lane's sidecars run under limits of their own, outside the
// lane's steps' room: admission holds them beside every lane, the first
// lane's (the lease's pod's) included, so a node never schedules past
// its total by a Postgres a lane (dl360, 2026-11-20).
func TestAdmissionHoldsEachLanesServices(t *testing.T) {
	rt := testRuntime(t)
	rt.Pods.Config.Limits = sandbox.Limits{CPUs: 8, Memory: "16g"}
	rt.Pods.Config.ServicesLimits = sandbox.Limits{CPUs: 2, Memory: "4g"}
	rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 64, MemGB: 256, Slots: 4})
	b := &meshapi.Brief{LeaseID: "lease-s", JobID: "job-s", Kind: "runsheet/validate", Resources: meshapi.Resources{CPUs: 4, MemGB: 8}}
	own, err := rt.Scheduler.TryTake(askOf(b, &meshapi.Lease{LeaseID: "lease-s", RenewEveryS: 30}))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Release()
	p := &leasePod{statuses: []sandbox.ServiceStatus{{Name: "postgres", Ready: true}}}
	svc := rt.servicesRoom(p)
	if svc.CPUs != 2 || svc.MemGB != 4 {
		t.Fatalf("services room %+v, want 2 cpus, 4 GB", svc)
	}
	if none := rt.servicesRoom(&leasePod{}); none.CPUs != 0 || none.MemGB != 0 {
		t.Fatalf("a pod with no services holds %+v for them", none)
	}
	got, err := rt.admit(context.Background(), b, sevenLanes(), svc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, a := range got {
			a.l.Release()
		}
	}()
	// 7 lanes of 4 cpus + 2 for each one's services = 42, the lease's own 4 among them
	if held := 64 - rt.Scheduler.Free().CPUs; held != 42 {
		t.Fatalf("the lease holds %g cpus, want 42 (7 lanes of 4, each with 2 for its services)", held)
	}
	if held := 256 - rt.Scheduler.Free().MemGB; held != 7*(8+4) {
		t.Fatalf("the lease holds %g GB, want %d", held, 7*(8+4))
	}
	// a lane's steps are still limited to its own room
	for _, a := range got {
		if a.room.CPUs != 4 {
			t.Fatalf("lane %s's room is %g cpus, want 4", a.name, a.room.CPUs)
		}
	}
}

// Only a lane whose suites run services holds the node's services
// budget beside its room (validate.Step.Services): a runsheet
// validation's checks and unit lanes start no Postgres, its two
// integration shards each do. With dl360's budget (services_cpus 4,
// services_memory 8g) four lanes of 4 cpus ask 16 + 2×4 = 24, less the
// lease's own 4: 20, not the 28 it asked when every lane held them.
func TestAdmissionHoldsServicesOnlyForLanesThatRunThem(t *testing.T) {
	rt := testRuntime(t)
	rt.Pods.Config.Limits = sandbox.Limits{CPUs: 8, Memory: "16g"}
	rt.Pods.Config.ServicesLimits = sandbox.Limits{CPUs: 4, Memory: "8g"}
	rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 88, MemGB: 256, Slots: 8})
	b := &meshapi.Brief{LeaseID: "lease-p", JobID: "job-p", Kind: "runsheet/validate", Resources: meshapi.Resources{CPUs: 4, MemGB: 8}}
	own, err := rt.Scheduler.TryTake(askOf(b, &meshapi.Lease{LeaseID: "lease-p", RenewEveryS: 30}))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Release()
	p := &leasePod{statuses: []sandbox.ServiceStatus{{Name: "postgres", Ready: true}}}
	room := validate.Resources{CPUs: 4, Memory: "8g"}
	sizes := []validate.LaneSize{
		{Name: "checks", Resources: room},
		{Name: "unit", Resources: room},
		{Name: "integration-0", Resources: room, Services: true},
		{Name: "integration-1", Resources: room, Services: true},
	}
	before := rt.Scheduler.Free()
	got, err := rt.admit(context.Background(), b, sizes, rt.servicesRoom(p))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, a := range got {
			a.l.Release()
		}
	}()
	after := rt.Scheduler.Free()
	if asked := before.CPUs - after.CPUs; asked != 20 {
		t.Fatalf("the lease asked %g cpus, want 20 (lanes 16 + services 2×4, less its own 4)", asked)
	}
	if asked := before.MemGB - after.MemGB; asked != 32+16-8 {
		t.Fatalf("the lease asked %g GB, want 40 (lanes 32 + services 2×8, less its own 8)", asked)
	}
	// three such validations fit on 88 cpus beside their own 4 each
	if held := 88 - after.CPUs; held != 24 {
		t.Fatalf("the lease holds %g cpus with its own, want 24", held)
	}
}

// A lane none of whose suites runs services holds no services room
// when it is opened on its own (holdLane), as at admission.
func TestLaneServicesOnlyForServiceLanes(t *testing.T) {
	rt := testRuntime(t)
	rt.Pods.Config.ServicesLimits = sandbox.Limits{CPUs: 4, Memory: "8g"}
	p := &leasePod{statuses: []sandbox.ServiceStatus{{Name: "postgres", Ready: true}}}
	if got := rt.laneServices(p, validate.LaneSize{Name: "checks"}); got.CPUs != 0 || got.MemGB != 0 {
		t.Fatalf("a lane without services holds %+v for them", got)
	}
	if got := rt.laneServices(p, validate.LaneSize{Name: "integration-0", Services: true}); got.CPUs != 4 || got.MemGB != 8 {
		t.Fatalf("a lane with services holds %+v for them, want 4 cpus, 8 GB", got)
	}
}
