package nodev1

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
	"projectmesh.io/mesh-cli/internal/validate"
)

// reserveRuntime is a node of 64 cpus, 200 GB keeping 40 cpus, 120 GB
// for its validation kinds, person of it for people's pull requests.
func reserveRuntime(t *testing.T, person scheduler.Capacity) *Runtime {
	t.Helper()
	rt := testRuntime(t)
	rt.Pods.Config.Limits = sandbox.Limits{CPUs: 8, Memory: "16g"}
	rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 64, MemGB: 200})
	rt.Scheduler.SetNamedReserve(ValidationReserveName, scheduler.Capacity{CPUs: 40, MemGB: 120}, person)
	rt.ValidationKinds = []string{"validate", "evaluate"}
	return rt
}

func validationBrief(id, subject, repo string) *meshapi.Brief {
	return &meshapi.Brief{LeaseID: id, JobID: "job-" + id, Kind: "runsheet/validate", Subject: meshapi.Subject{Type: subject, ID: id},
		Code: &meshapi.Code{Repo: meshapi.Repo{FullName: repo}}, Resources: meshapi.Resources{CPUs: 4, MemGB: 8}}
}

func lanesOf(prefix string, n int, cpus float64, mem string) []validate.LaneSize {
	out := make([]validate.LaneSize, n)
	for i := range out {
		out[i] = validate.LaneSize{Name: fmt.Sprintf("%s-%d", prefix, i), Resources: validate.Resources{CPUs: cpus, Memory: mem}}
	}
	return out
}

func leaseOf(id string) *meshapi.Lease { return &meshapi.Lease{LeaseID: id, RenewEveryS: 30} }

// Goal a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use,
// acceptance check 1 (P1): builds fill all the room the reserve leaves,
// then a chain validation is offered; the take names the reserve for
// the validation kinds, the validation's lease and every lane are
// admitted at once into it, and a further build waits.
func TestAValidationReserveIsNeverTakenByBuilds(t *testing.T) {
	rt := reserveRuntime(t, scheduler.Capacity{})
	build := func(i int) scheduler.Ask {
		b := &meshapi.Brief{LeaseID: fmt.Sprintf("build-%d", i), Kind: "runsheet/build", Resources: meshapi.Resources{CPUs: 4, MemGB: 8}}
		return rt.leaseAsk(b, leaseOf(b.LeaseID))
	}
	builds := 0
	for ; ; builds++ {
		if _, err := rt.Scheduler.TryTake(build(builds)); err != nil {
			if !errors.Is(err, scheduler.ErrNoRoom) {
				t.Fatal(err)
			}
			break
		}
	}
	if builds != 6 {
		t.Fatalf("%d builds admitted, want 6: the 24 cpus the reserve leaves", builds)
	}
	if f := rt.Scheduler.Free(); f.CPUs != 0 {
		t.Fatalf("free %g cpus with the general room full, want 0", f.CPUs)
	}
	if f := rt.Scheduler.ReserveFreeOf(ValidationReserveName); f.CPUs != 40 || f.MemGB != 120 {
		t.Fatalf("the reserve has %+v free after the builds, want all of it: builds never take it", f)
	}

	// Mesh is told the reserve is there for the validation kinds alone
	tr := meshapi.TakeRequest{Free: rt.free()}
	rt.takeReserved(&tr, false)
	if tr.Reserved == nil || tr.Reserved.CPUs != 40 || !slices.Contains(tr.ReservedKinds, "validate") || slices.Contains(tr.ReservedKinds, "build") {
		t.Fatalf("the take names reserved %+v for %v, want 40 cpus for the validation kinds", tr.Reserved, tr.ReservedKinds)
	}

	// a chain validation: its own hold and its three 8-cpu lanes are
	// admitted at once, all in the reserve
	b := validationBrief("chain", "candidate", "runsheet/runsheet")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	own, err := rt.holdCapacity(ctx, b, leaseOf(b.LeaseID))
	if err != nil {
		t.Fatalf("the chain validation's own hold: %v", err)
	}
	defer own.Release()
	lanes, err := rt.admit(ctx, b, lanesOf("integration", 3, 8, "16g"), meshapi.Resources{})
	if err != nil {
		t.Fatalf("the chain validation's lanes: %v", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("the chain validation waited %s, want admitted at once", waited)
	}
	if f := rt.Scheduler.ReserveFreeOf(ValidationReserveName); f.CPUs != 16 {
		t.Fatalf("the reserve has %g cpus free, want 16: the validation holds 24 of it", f.CPUs)
	}
	for _, h := range rt.Scheduler.Leases() {
		if strings.HasPrefix(h.Owner, "chain") && h.Ask.Reserve != ValidationReserveName {
			t.Fatalf("%s holds %+v, want the validation reserve drawn", h.Owner, h.Ask)
		}
	}

	// a further build waits: the reserve's 16 free cpus are not its
	if _, err := rt.Scheduler.TryTake(build(builds)); !errors.Is(err, scheduler.ErrNoRoom) {
		t.Fatalf("a further build: err %v, want no room", err)
	}
	for _, l := range lanes {
		l.l.Release()
	}
}

// Goal a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use,
// acceptance check 2 (P2): the reserve is full of automated item
// validations, the next automated one waits for its lanes, and a
// person's pull request validation arrives: it is admitted at once into
// the person share, ahead of the waiting automated validation, which
// is admitted once room frees.
func TestAHandPRValidationIsNotStarved(t *testing.T) {
	rt := reserveRuntime(t, scheduler.Capacity{CPUs: 12, MemGB: 36})
	// builds fill the general room
	for i := 0; i < 6; i++ {
		if _, err := rt.Scheduler.TryTake(scheduler.Ask{Owner: fmt.Sprintf("build-%d", i), CPUs: 4, MemGB: 8, Slot: true}); err != nil {
			t.Fatal(err)
		}
	}
	// automated item validations fill the reserve but its person share:
	// 28 cpus, each 4 of its own and 3 lanes beside
	var held []*scheduler.Lease
	for i := 0; i < 2; i++ {
		b := validationBrief(fmt.Sprintf("item-%d", i), "item", "runsheet/runsheet")
		own, err := rt.holdCapacity(context.Background(), b, leaseOf(b.LeaseID))
		if err != nil {
			t.Fatalf("item validation %d's own hold: %v", i, err)
		}
		lanes, err := rt.admit(context.Background(), b, lanesOf("unit", 3, 14.0/3, "8g"), meshapi.Resources{})
		if err != nil {
			t.Fatalf("item validation %d's lanes: %v", i, err)
		}
		held = append(held, own)
		for _, l := range lanes {
			held = append(held, l.l)
		}
	}
	if f := rt.Scheduler.ReserveFreeFor(ValidationReserveName, false); f.CPUs > 1e-6 {
		t.Fatalf("the reserve has %g cpus free for automated validations, want none", f.CPUs)
	}

	// the next automated validation's own hold fits nowhere: Mesh is not
	// told of the reserve for it, so it would not be placed; its lanes,
	// had it been, wait
	next := validationBrief("item-next", "item", "runsheet/runsheet")
	if _, err := rt.Scheduler.TryTake(rt.leaseAsk(next, leaseOf(next.LeaseID))); !errors.Is(err, scheduler.ErrNoRoom) {
		t.Fatalf("an automated validation took the person share: err %v", err)
	}
	waiting := make(chan []admittedLane, 1)
	go func() {
		ls, _ := rt.admit(context.Background(), next, lanesOf("unit", 2, 4, "8g"), meshapi.Resources{})
		waiting <- ls
	}()
	waitFor(t, "the automated validation to wait", func() bool { return len(rt.Scheduler.Waiting()) == 1 })

	// the person's pull request validation is admitted at once, in the
	// person share, ahead of the automated one waiting before it
	hand := validationBrief("hand-pr", "pull_request", "runsheet/mesh-cli")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	own, err := rt.holdCapacity(ctx, hand, leaseOf(hand.LeaseID))
	if err != nil {
		t.Fatalf("the hand PR validation's own hold: %v", err)
	}
	lanes, err := rt.admit(ctx, hand, lanesOf("unit", 2, 4, "8g"), meshapi.Resources{})
	if err != nil {
		t.Fatalf("the hand PR validation's lanes, with an automated one waiting: %v", err)
	}
	if f := rt.Scheduler.ReserveFreeFor(ValidationReserveName, true); f.CPUs != 4 {
		t.Fatalf("the person share has %g cpus free, want 4: the hand PR holds 8 of its 12", f.CPUs)
	}
	select {
	case <-waiting:
		t.Fatal("the automated validation was admitted into the person share")
	default:
	}
	// the holding report names the reserve the automated one waits on
	if ws := rt.Status().Waiting; len(ws) != 1 || ws[0].LeaseID != "item-next" || ws[0].Reserve != ValidationReserveName || !strings.Contains(ws[0].Room, "validations reserve") {
		t.Fatalf("the holding report: %+v, want item-next waiting on the validations reserve", ws)
	}

	// room frees: the automated validation is admitted
	for _, l := range held {
		l.Release()
	}
	select {
	case ls := <-waiting:
		if len(ls) != 2 {
			t.Fatalf("the automated validation was admitted %d lanes, want 2", len(ls))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the automated validation still waits with room free")
	}
	own.Release()
	for _, l := range lanes {
		l.l.Release()
	}
}

// With no validation reserve set the node admits as before: a
// validation's asks name no reserve and no person, and the take names
// none for it.
func TestNoValidationReserveAdmitsAsBefore(t *testing.T) {
	rt := testRuntime(t)
	rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 64, MemGB: 200})
	rt.ValidationKinds = []string{"validate"}
	b := validationBrief("v", "pull_request", "runsheet/runsheet")
	if a := rt.leaseAsk(b, leaseOf("v")); a.Reserve != "" || a.Person || a.Repo != "" {
		t.Fatalf("no reserve set: the ask %+v names one", a)
	}
	tr := meshapi.TakeRequest{Free: rt.free()}
	rt.takeReserved(&tr, false)
	if tr.Reserved != nil || tr.ReservedKinds != nil {
		t.Fatalf("no reserve set: the take names %+v %v", tr.Reserved, tr.ReservedKinds)
	}
}

// node.yaml's v1.validation_reserve overrides the flags field by field.
func TestReadValidationReserve(t *testing.T) {
	p := filepath.Join(t.TempDir(), "node.yaml")
	def := ValidationReserve{Kinds: []string{"validate", "evaluate"}}
	if got, err := ReadValidationReserve(p, def); err != nil || got.CPUs != 0 {
		t.Fatalf("no file: %+v %v", got, err)
	}
	os.WriteFile(p, []byte("v1:\n  validation_reserve:\n    cpus: 40\n    mem_gb: 120\n    person_cpus: 12\n"), 0o600)
	got, err := ReadValidationReserve(p, def)
	if err != nil || got.CPUs != 40 || got.MemGB != 120 || got.PersonCPUs != 12 || got.PersonMemGB != 0 || len(got.Kinds) != 2 {
		t.Fatalf("read: %+v %v", got, err)
	}
}

// fakeMeter is a UsageMeter whose containers each use cores CPUs from
// their first reading on, and hold mem now and peak bytes.
type fakeMeter struct {
	mu    sync.Mutex
	first time.Time
	// containers by name: cores, now, peak
	cs map[string][3]float64
}

func (m *fakeMeter) Read(context.Context) ([]ContainerUse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.first.IsZero() {
		m.first = now
	}
	var out []ContainerUse
	for name, c := range m.cs {
		out = append(out, ContainerUse{Name: name, CPUUsec: uint64(c[0] * 1e6 * now.Sub(m.first).Seconds()), MemNow: uint64(c[1]), MemPeak: uint64(c[2])})
	}
	return out, nil
}

// Goal a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use,
// acceptance check 3 (P3): two leases of a kind run at once, their
// containers read while they run; once they finish, the node's report
// has the kind's CPU and memory peaks and that two ran at once.
func TestLeasePeaksAreRecordedByKind(t *testing.T) {
	sess := &stubSession{calls: []string{"conform/submit"}, hold: make(chan struct{})}
	g := newRig(t, sess)
	g.rt.Once = false
	g.rt.Node.CapacityTotal = meshapi.Capacity{CPUs: 16, MemGB: 32}
	g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 16, MemGB: 32})
	meter := &fakeMeter{cs: map[string][3]float64{}}
	g.rt.Meter = meter
	g.offer(t, func(b *meshapi.Brief) { b.Resources = meshapi.Resources{CPUs: 2, MemGB: 4} })
	g.offer(t, func(b *meshapi.Brief) { b.Resources = meshapi.Resources{CPUs: 2, MemGB: 4} })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()
	waitFor(t, "both sessions", func() bool { return len(sess.specsSoFar()) == 2 })

	st := g.rt.Status()
	if len(st.Leases) != 2 {
		t.Fatalf("%d leases running, want 2", len(st.Leases))
	}
	kind := path.Base(st.Leases[0].Kind)
	const gib = 1 << 30
	meter.mu.Lock()
	// one lease: an agent of 2 cpus with a 3 GiB peak; the other: an
	// agent and a postgres, 5 GiB held together at once
	meter.cs[PodName(st.Leases[0].LeaseID)+"-agent"] = [3]float64{2, 1 * gib, 3 * gib}
	meter.cs[PodName(st.Leases[1].LeaseID)+"-agent"] = [3]float64{1, 3 * gib, 3.5 * gib}
	meter.cs[PodName(st.Leases[1].LeaseID)+"-postgres"] = [3]float64{0.5, 2 * gib, 2 * gib}
	meter.cs["someone-elses-container"] = [3]float64{40, 100 * gib, 100 * gib}
	meter.mu.Unlock()
	g.rt.readPeaks(ctx)
	time.Sleep(300 * time.Millisecond)
	g.rt.readPeaks(ctx)

	close(sess.hold)
	waitFor(t, "both finishes", func() bool { return len(g.fake.Calls("finish")) == 2 })
	waitFor(t, "both leases out of the registry", func() bool { return g.rt.leases.Len() == 0 })
	var u *KindUsage
	for _, k := range g.rt.Status().Usage {
		if k.Kind == kind {
			u = &k
		}
	}
	if u == nil {
		t.Fatalf("the report has no usage of kind %s: %+v", kind, g.rt.Status().Usage)
	}
	if u.Ended != 2 || u.Running != 0 || u.MaxAtOnce != 2 || u.Measured != 2 {
		t.Fatalf("usage %+v, want 2 ended, 2 at once, 2 measured", *u)
	}
	if u.CPUMax < 1.8 || u.CPUMax > 2.2 || u.CPUP50 < 1.3 || u.CPUP50 > 1.7 {
		t.Fatalf("cpu peaks %+v, want the highest about 2 and the median about 1.5 cores", *u)
	}
	if u.MemGBMax != 5 || u.MemGBP50 != 3 {
		t.Fatalf("memory peaks %+v, want the highest 5 GB (two containers at once) and the median 3", *u)
	}
	if lines := strings.Join(g.rt.Status().DrainLines(time.Now()), "\n"); !strings.Contains(lines, kind+"  2 ended, 0 running, 2 at most at once; 2 measured") {
		t.Fatalf("mesh node status does not report the kind's usage:\n%s", lines)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime did not stop")
	}
}

// Goal a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use
// P5: a lease waiting for room is in the holding report naming the
// room it waits on — the validation reserve, the assistant's, or the
// general room alone — and node.log's wait line names it too.
func TestHoldingNamesTheReserveALeaseWaitsFor(t *testing.T) {
	rt := reserveRuntime(t, scheduler.Capacity{})
	rt.Scheduler.SetReserve(scheduler.Capacity{CPUs: 4, MemGB: 8})
	rt.AssistantKinds = []string{"goal-chat"}
	var mu sync.Mutex
	var logged []string
	rt.Logf = func(format string, args ...any) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	// everything full: general room, the validation reserve, the assistant's
	for _, a := range []scheduler.Ask{
		{Owner: "builds", CPUs: 20, MemGB: 50},
		{Owner: "validations", CPUs: 40, MemGB: 120, Reserve: ValidationReserveName},
		{Owner: "chats", CPUs: 4, MemGB: 8, Reserved: true},
	} {
		if _, err := rt.Scheduler.TryTake(a); err != nil {
			t.Fatalf("%s: %v", a.Owner, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, b := range []*meshapi.Brief{
		validationBrief("chain", "candidate", "runsheet/runsheet"),
		{LeaseID: "chat", Kind: "runsheet/goal-chat", Resources: meshapi.Resources{CPUs: 2, MemGB: 4}},
		{LeaseID: "build", Kind: "runsheet/build", Resources: meshapi.Resources{CPUs: 4, MemGB: 8}},
	} {
		go rt.holdCapacity(ctx, b, leaseOf(b.LeaseID))
	}
	waitFor(t, "three waiters", func() bool { return len(rt.Scheduler.Waiting()) == 3 })
	st := rt.Status()
	rooms := map[string]string{}
	for _, w := range st.Waiting {
		rooms[w.LeaseID] = w.Reserve + "|" + w.Room
	}
	if !strings.HasPrefix(rooms["chain"], "validations|the validations reserve") ||
		!strings.HasPrefix(rooms["chat"], "assistant|the assistant reserve") ||
		rooms["build"] != "|the general room" {
		t.Fatalf("the holding report's rooms: %v", rooms)
	}
	lines := strings.Join(st.DrainLines(time.Now()), "\n")
	for _, want := range []string{"Waiting for room: 3:", "on the validations reserve, then the general room", "Reserve validations: 40 cpus, 120 GB, 0 cpus, 0 GB free"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("mesh node status lacks %q:\n%s", want, lines)
		}
	}
	mu.Lock()
	said := strings.Join(logged, "\n")
	mu.Unlock()
	if !strings.Contains(said, "lease chain (validate) waits for room in the validations reserve") {
		t.Fatalf("node.log does not name the reserve:\n%s", said)
	}
}

// A lease's first reading is a baseline: the CPU its containers used
// before it (a lease adopted across a restart, its containers hours
// old) is never read as one window's, and the next reading's window is
// what it used since.
func TestALeasesFirstReadingIsABaseline(t *testing.T) {
	var p leasePeaks
	t0 := time.Now()
	// adopted: an agent that has used 2 hours of CPU since long before
	p.add(t0, []ContainerUse{{Name: "a", CPUUsec: 7200e6, MemNow: 1 << 30}})
	if p.cpu != 0 || p.mem != 1<<30 {
		t.Fatalf("after the first reading: cpu %g, mem %d; want no cpu, the memory read", p.cpu, p.mem)
	}
	// 10 s later it has used 20 s more: 2 cores; a step started since
	// used 5 s: half a core
	p.add(t0.Add(10*time.Second), []ContainerUse{{Name: "a", CPUUsec: 7220e6}, {Name: "step", CPUUsec: 5e6}})
	if p.cpu < 2.49 || p.cpu > 2.51 {
		t.Fatalf("cpu peak %g after the second reading, want 2.5 cores", p.cpu)
	}
}
