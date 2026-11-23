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

// A node at full validation load still runs an assistant's lease at once
// (goal one-assistant-dispatcher-per-org-answers-every-ask P1,
// acceptance check 1): its take names the room it keeps for the
// assistant kinds, with no general room free; the goal-chat lease is
// held in that reserve, beside the validation, which keeps its room.
func TestAFullNodeAdmitsAnAssistantLease(t *testing.T) {
	sess := &stubSession{calls: []string{"conform/submit"}, hold: make(chan struct{})}
	g := newRig(t, sess)
	g.rt.Once = false
	g.rt.Node.CapacityTotal = meshapi.Capacity{CPUs: 16, MemGB: 32}
	g.rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 16, MemGB: 32})
	g.rt.Scheduler.SetReserve(scheduler.Capacity{CPUs: 4, MemGB: 8})
	g.rt.AssistantKinds = []string{"goal-chat"}
	// a validation holds the whole general room
	v, err := g.rt.Scheduler.TryTake(scheduler.Ask{Owner: "validate", CPUs: 12, MemGB: 24, Slot: true, Cache: scheduler.CacheShared})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Release()
	g.offer(t, func(b *meshapi.Brief) {
		b.Kind = "runsheet/goal-chat"
		b.Resources = meshapi.Resources{CPUs: 2, MemGB: 4}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()
	waitFor(t, "the assistant's session", func() bool { return len(sess.specsSoFar()) == 1 })

	var first meshapi.TakeRequest
	json.Unmarshal(g.fake.Calls("take")[0].Body, &first)
	if f := first.Free; f == nil || f.CPUs != 0 || f.MemGB != 0 {
		t.Fatalf("the take's free %+v, want none: the reserve is never offered to other kinds", f)
	}
	if r := first.Reserved; r == nil || r.CPUs != 4 || r.MemGB != 8 || len(first.ReservedKinds) != 1 || first.ReservedKinds[0] != "goal-chat" {
		t.Fatalf("the take's reserved %+v for %v, want 4 cpus, 8 GB for goal-chat", r, first.ReservedKinds)
	}
	var chat *scheduler.Held
	for _, h := range g.rt.Scheduler.Leases() {
		if h.Owner != "validate" {
			chat = &h
		}
	}
	if chat == nil || !chat.Ask.Reserved || chat.Ask.Slot || chat.Ask.Cache != scheduler.CacheNone {
		t.Fatalf("the assistant's hold %+v, want one in the reserve, no slot, no cache", chat)
	}
	if f := g.rt.Scheduler.ReserveFree(); f.CPUs != 2 || f.MemGB != 4 {
		t.Fatalf("ReserveFree %+v after the assistant's lease, want 2 cpus, 4 GB", f)
	}
	if hb := g.fake.Calls("hand-back"); len(hb) != 0 {
		t.Fatalf("hand-backs %+v, want none", hb)
	}
	close(sess.hold)
	waitFor(t, "the finish", func() bool { return len(g.fake.Calls("finish")) == 1 })
	waitFor(t, "the reserve back", func() bool { return g.rt.Scheduler.ReserveFree().CPUs == 4 })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime did not stop")
	}
}

// Only the assistant kinds ask for the reserve: a validation's lease
// asks for the general room as before, and a node keeping no reserve
// names none on its takes.
func TestOnlyAssistantKindsAskForTheReserve(t *testing.T) {
	r := &Runtime{AssistantKinds: []string{"goal-chat"}, Scheduler: scheduler.New(scheduler.Capacity{CPUs: 16, MemGB: 32})}
	lease := &meshapi.Lease{LeaseID: "l", RenewEveryS: 30}
	if a := r.leaseAsk(&meshapi.Brief{Kind: "runsheet/validate"}, lease); a.Reserved || !a.Slot || a.Cache != scheduler.CacheShared {
		t.Fatalf("a validation's ask %+v, want the general room's", a)
	}
	if a := r.leaseAsk(&meshapi.Brief{Kind: "runsheet/goal-chat"}, lease); !a.Reserved {
		t.Fatalf("a goal-chat's ask %+v, want the reserve", a)
	}
	var tr meshapi.TakeRequest
	r.takeReserved(&tr, false)
	if tr.Reserved != nil || tr.ReservedKinds != nil {
		t.Fatalf("no reserve set: the take names %+v %v", tr.Reserved, tr.ReservedKinds)
	}
	r.Scheduler.SetReserve(scheduler.Capacity{CPUs: 4, MemGB: 8})
	tr = meshapi.TakeRequest{Free: &meshapi.Capacity{CPUs: 12, MemGB: 24}}
	r.takeReserved(&tr, true)
	if tr.Reserved == nil || tr.Free.CPUs != 0 {
		t.Fatalf("a hot host's take: free %+v reserved %+v, want the reserve alone", tr.Free, tr.Reserved)
	}
}

// node.yaml's v1.assistant_reserve overrides the flags field by field.
func TestReadAssistantReserve(t *testing.T) {
	p := filepath.Join(t.TempDir(), "node.yaml")
	def := AssistantReserve{CPUs: 4, MemGB: 8, Kinds: []string{"goal-chat"}}
	if got, err := ReadAssistantReserve(p, def); err != nil || got.CPUs != 4 {
		t.Fatalf("no file: %+v %v", got, err)
	}
	os.WriteFile(p, []byte("v1:\n  assistant_reserve:\n    cpus: 6\n"), 0o600)
	got, err := ReadAssistantReserve(p, def)
	if err != nil || got.CPUs != 6 || got.MemGB != 8 || len(got.Kinds) != 1 {
		t.Fatalf("cpus only: %+v %v", got, err)
	}
}
