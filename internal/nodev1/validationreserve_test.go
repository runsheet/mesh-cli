package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// TestAValidationReserveIsNeverTakenByBuilds (goal
// a-v1-node-reserves-room-for-validations-and-sizes-its-reserves-from-use
// P1, acceptance check 1): builds fill all of the node's general room,
// leaving only the validation reserve open; a chain validation offered
// after it is still admitted at once, into the reserve the builds never
// drew from, keeping its job slot and shared cache hold like any other
// build; a further build offered after that still waits for room, the
// reserve never theirs to take either.
func TestAValidationReserveIsNeverTakenByBuilds(t *testing.T) {
	var rt *Runtime
	found := make(chan scheduler.Ask, 1)
	sess := &stubSession{calls: []string{"conform/submit"}, inspect: func(spec SessionSpec) {
		for _, h := range rt.Scheduler.Leases() {
			if h.Ask.Reserve == scheduler.ReserveValidation {
				found <- h.Ask
				return
			}
		}
		found <- scheduler.Ask{}
	}}
	g := newRig(t, sess)
	rt = g.rt
	g.rt.Once = false
	g.rt.Node.CapacityTotal = meshapi.Capacity{CPUs: 16, MemGB: 32}
	g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 16, MemGB: 32})
	g.rt.Scheduler.SetReserve(scheduler.ReserveValidation, scheduler.Capacity{CPUs: 4, MemGB: 8})
	g.rt.ValidationKinds = []string{"validate"}

	// builds fill the node's whole general room directly (as
	// TestAFullNodeAdmitsAnAssistantLease fills it for the assistant
	// reserve's own test), leaving only the validation reserve open
	b1, err := g.rt.Scheduler.TryTake(scheduler.Ask{Owner: "build-1", CPUs: 8, MemGB: 16, Slot: true, Cache: scheduler.CacheShared})
	if err != nil {
		t.Fatal(err)
	}
	defer b1.Release()
	b2, err := g.rt.Scheduler.TryTake(scheduler.Ask{Owner: "build-2", CPUs: 4, MemGB: 8, Slot: true, Cache: scheduler.CacheShared})
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Release()
	if f := g.rt.Scheduler.Free(); f.CPUs != 0 || f.MemGB != 0 {
		t.Fatalf("Free = %+v, want none: the builds fill the whole general room", f)
	}

	g.offer(t, func(b *meshapi.Brief) {
		b.Kind = "runsheet/validate"
		b.Subject = meshapi.Subject{Type: "candidate", ID: "cand-1"}
		b.Resources = meshapi.Resources{CPUs: 2, MemGB: 4}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()

	var ask scheduler.Ask
	select {
	case ask = <-found:
	case <-time.After(10 * time.Second):
		t.Fatal("the validation's session never started")
	}
	if ask.Reserve != scheduler.ReserveValidation || !ask.Slot || ask.Cache != scheduler.CacheShared {
		t.Fatalf("the validation's hold %+v, want one in the validation reserve, with a slot and the shared cache (a real build, not a lightweight assistant ask)", ask)
	}

	// a further build offered after it still waits for room: the general
	// room the builds filled is not the validation reserve's to give, and
	// it is not yet handed back while it waits (holdCapacity is still
	// blocked in the scheduler, not yet answered)
	job2 := g.offer(t, func(b *meshapi.Brief) { b.Resources = meshapi.Resources{CPUs: 1, MemGB: 1} })
	waitFor(t, "the second build's lease", func() bool { return g.fake.LeaseOf(job2) != "" })
	for _, c := range g.fake.Calls("hand-back") {
		if c.LeaseID == g.fake.LeaseOf(job2) {
			t.Fatalf("the second build was handed back already: %+v, want it still waiting for room", c)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime did not stop")
	}
	// once its wait ends (here, with the runtime's own ctx), the second
	// build hands back cannot_run rather than being granted the reserve
	var hbReq meshapi.HandBackRequest
	gotHB := false
	for _, c := range g.fake.Calls("hand-back") {
		if c.LeaseID == g.fake.LeaseOf(job2) {
			json.Unmarshal(c.Body, &hbReq)
			gotHB = true
		}
	}
	if !gotHB || hbReq.Cause != meshapi.CauseCannotRun {
		t.Fatalf("the second build's hand-back %+v (seen %v), want cause cannot_run", hbReq, gotHB)
	}
}

// node.yaml's v1.validation_reserve overrides the flags field by field,
// mirroring v1.assistant_reserve (TestReadAssistantReserve).
func TestReadValidationReserve(t *testing.T) {
	p := filepath.Join(t.TempDir(), "node.yaml")
	def := ValidationReserve{CPUs: 4, MemGB: 8, Kinds: []string{"validate", "evaluate"}}
	if got, err := ReadValidationReserve(p, def); err != nil || got.CPUs != 4 {
		t.Fatalf("no file: %+v %v", got, err)
	}
	os.WriteFile(p, []byte("v1:\n  validation_reserve:\n    cpus: 6\n"), 0o600)
	got, err := ReadValidationReserve(p, def)
	if err != nil || got.CPUs != 6 || got.MemGB != 8 || len(got.Kinds) != 2 {
		t.Fatalf("cpus only: %+v %v", got, err)
	}
}
