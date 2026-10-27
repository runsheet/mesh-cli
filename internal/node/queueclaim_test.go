package node

import (
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// queueItem3 is a third ready item the slot test adds.
const queueItem3 = "abababab-2222-4222-8222-abababababab"

// queueRig is an owned node of one slot on seed's fake (item1 ready),
// recording the items it dispatches.
type queueRig struct {
	f          *gatewaytest.Fake
	loop       *Loop
	logs       *syncBuf
	mu         sync.Mutex
	dispatched []string
}

func newQueueRig(t *testing.T, capacity int) *queueRig {
	t.Helper()
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(capacity)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "n"}); err != nil {
		t.Fatal(err)
	}
	r := &queueRig{f: f, logs: &syncBuf{}}
	r.loop = &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: log.New(r.logs, "", 0),
		Owned: &Owned{NodeID: cfg.NodeID},
		Dispatch: func(_ context.Context, it gateway.WorkItem) {
			r.mu.Lock()
			r.dispatched = append(r.dispatched, it.ID)
			r.mu.Unlock()
		}}
	return r
}

func (r *queueRig) tick() {
	r.loop.Tick(context.Background())
	r.loop.wg.Wait()
}

// oneClaims are the claimJob calls that named no subject.
func (r *queueRig) oneClaims() int {
	n := 0
	for _, c := range r.f.CallsNamed("claimJob") {
		if _, named := c.Input["subject"]; !named {
			n++
		}
	}
	return n
}

// With the one claim, a free slot asks Mesh for the next job and runs
// the build it is handed: one claim, the item claimed through its own
// kind's claim, dispatched once.
func TestAFreeSlotClaimsTheNextJobFromTheQueue(t *testing.T) {
	r := newQueueRig(t, 1)
	r.f.Queue = []gatewaytest.QueuedJob{{Kind: "runsheet/build", SubjectType: "item", SubjectID: item1}}
	r.tick()
	if got := r.dispatched; len(got) != 1 || got[0] != item1 {
		t.Fatalf("dispatched %v, want [%s]\n%s", got, item1, r.logs.String())
	}
	if !strings.Contains(r.logs.String(), "from the queue") {
		t.Fatalf("no queue claim line:\n%s", r.logs.String())
	}
	if n := len(r.f.Claims); n != 1 {
		t.Fatalf("claimWorkItem asked %d times, want 1 (through the one claim)", n)
	}
}

// One claim per free slot: two slots, two offers, two builds started;
// a third offer waits for the next tick.
func TestOneClaimPerFreeSlot(t *testing.T) {
	r := newQueueRig(t, 2)
	r.f.WorkItems[item2]["status"], r.f.WorkItems[item2]["dependsOn"] = "ready", []any{}
	third := map[string]any{}
	for k, v := range r.f.WorkItems[item2] {
		third[k] = v
	}
	third["id"], third["seq"] = queueItem3, 3
	r.f.WorkItems[queueItem3] = third
	r.f.Queue = []gatewaytest.QueuedJob{
		{Kind: "runsheet/build", SubjectType: "item", SubjectID: item1},
		{Kind: "runsheet/build", SubjectType: "item", SubjectID: item2},
		{Kind: "runsheet/build", SubjectType: "item", SubjectID: queueItem3},
	}
	r.tick()
	if got := len(r.dispatched); got != 2 {
		t.Fatalf("dispatched %d builds with 2 free slots, want 2\n%s", got, r.logs.String())
	}
	if len(r.f.Queue) != 1 {
		t.Fatalf("queue left %d, want 1 for the next tick", len(r.f.Queue))
	}
}

// A platform without the one claim: the node says so once, claims kind
// by kind this tick as before, and does not ask again for queueRetry.
func TestWithoutTheOneClaimTheNodeClaimsKindByKind(t *testing.T) {
	r := newQueueRig(t, 1)
	r.tick()
	if got := r.dispatched; len(got) != 1 || got[0] != item1 {
		t.Fatalf("dispatched %v, want [%s] claimed kind by kind\n%s", got, item1, r.logs.String())
	}
	if !strings.Contains(r.logs.String(), "the platform has no one claim") {
		t.Fatalf("no fallback line:\n%s", r.logs.String())
	}
	asked := r.oneClaims()
	r.tick()
	if r.oneClaims() != asked {
		t.Fatalf("asked the one claim again within queueRetry")
	}
}

// A job the node cannot run is handed back, refused with why, not run.
func TestAQueuedBuildTheNodeCannotRunIsHandedBack(t *testing.T) {
	r := newQueueRig(t, 1)
	r.loop.Declines = &Declines{}
	r.loop.Declines.HoldFor(item1, "needs xcode", time.Now(), time.Hour)
	r.f.Queue = []gatewaytest.QueuedJob{{Kind: "runsheet/build", SubjectType: "item", SubjectID: item1}}
	r.tick()
	if len(r.dispatched) != 0 {
		t.Fatalf("dispatched %v, want nothing", r.dispatched)
	}
	rel := r.f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 {
		t.Fatalf("releaseWorkItem asked %d times, want 1\n%s", len(rel), r.logs.String())
	}
	if note, _ := r.f.Item(item1)["lastNote"].(string); !strings.Contains(note, "refused: cannot run") {
		t.Fatalf("item's note %q, want refused: cannot run", note)
	}
}

// An empty queue claims nothing and starts nothing.
func TestAnEmptyQueueStartsNothing(t *testing.T) {
	r := newQueueRig(t, 1)
	r.f.Queue = []gatewaytest.QueuedJob{}
	r.tick()
	if len(r.dispatched) != 0 {
		t.Fatalf("dispatched %v from an empty queue", r.dispatched)
	}
	if n := len(r.f.Claims); n != 0 {
		t.Fatalf("claimWorkItem asked %d times with an empty queue", n)
	}
}
