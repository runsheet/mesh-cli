package node

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// budgetPaused is the Mini's refusal of 2026-10-03, asked 285 times at
// 70 seconds (ADR-024).
const budgetPaused = "goal is paused: budget exhausted"

// claimHoldRig is a node on seed's fake, one item ready (item1), its
// claim answered refusal, on a fake clock as headroomRig's.
type claimHoldRig struct {
	f    *gatewaytest.Fake
	loop *Loop
	logs *syncBuf
	now  *time.Time
}

func newClaimHoldRig(t *testing.T, refusal string) *claimHoldRig {
	t.Helper()
	f := seed(t)
	f.Goals[goalID]["updatedAt"] = "2026-10-03T04:00:00Z"
	f.ClaimRefusals = map[string]string{item1: refusal}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "n"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 3, 4, 0, 0, 0, time.UTC)
	r := &claimHoldRig{f: f, logs: &syncBuf{}, now: &now}
	r.loop = &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: log.New(r.logs, "", 0),
		Now: func() time.Time { return *r.now }, Dispatch: func(context.Context, gateway.WorkItem) {}}
	return r
}

// tick moves the clock 70 seconds and ticks once.
func (r *claimHoldRig) tick(t *testing.T) {
	t.Helper()
	*r.now = r.now.Add(70 * time.Second)
	r.loop.Tick(context.Background())
	r.loop.wg.Wait()
}

// fiveHours ticks every 70 seconds for five simulated hours.
func (r *claimHoldRig) fiveHours(t *testing.T) {
	t.Helper()
	for end := r.now.Add(5 * time.Hour); r.now.Before(end); {
		r.tick(t)
	}
}

// claims is every claimWorkItem the fake was asked, refused or not.
func (r *claimHoldRig) claims() int { return len(r.f.Claims) }

// held asserts the goal is held as kind item with the classed reason.
func (r *claimHoldRig) held(t *testing.T) {
	t.Helper()
	h := r.loop.HeldGoals()
	if len(h) != 1 || h[0].GoalID != goalID || h[0].Slug != "ship-it" || h[0].Kind != "item" || h[0].Reason != "budget: "+budgetPaused {
		t.Fatalf("HeldGoals = %+v, want ship-it held (item) for %q", h, "budget: "+budgetPaused)
	}
}

func (r *claimHoldRig) released(t *testing.T) {
	t.Helper()
	if h := r.loop.HeldGoals(); len(h) != 0 {
		t.Fatalf("HeldGoals = %+v after the hold lifted, want none", h)
	}
	if !strings.Contains(r.logs.String(), "claim: acme goal ship-it hold lifted") {
		t.Fatalf("no lifted line:\n%s", r.logs.String())
	}
}

// A budget-paused claim is asked once in five simulated hours and said
// once; a goal row changed (updatedAt) lifts the hold and the next tick
// claims again.
func TestClaimRefusedForBudgetIsHeldUntilTheGoalChanges(t *testing.T) {
	r := newClaimHoldRig(t, budgetPaused)
	r.fiveHours(t)
	if n := r.claims(); n != 1 {
		t.Fatalf("claimWorkItem asked %d times in five hours, want 1", n)
	}
	logs := r.logs.String()
	if n := strings.Count(logs, "held (budget)"); n != 1 {
		t.Fatalf("%d held (budget) lines, want 1:\n%s", n, logs)
	}
	if !strings.Contains(logs, "claim: acme goal ship-it held (budget): "+budgetPaused+"; not asked again until the goal changes") {
		t.Fatalf("no held line:\n%s", logs)
	}
	r.held(t)
	if h := r.loop.Holding(); h == nil || len(h.Unclaimed) != 1 || !strings.Contains(h.Unclaimed[0].Reason, budgetPaused) {
		t.Fatalf("holding = %+v, want the goal passed over for its hold", h)
	}

	delete(r.f.ClaimRefusals, item1)
	r.f.Goals[goalID]["updatedAt"] = "2026-10-03T09:30:00Z"
	r.tick(t)
	if n := r.claims(); n != 2 {
		t.Fatalf("claimWorkItem asked %d times after the goal changed, want 2", n)
	}
	if r.f.Item(item1)["status"] != "claimed" {
		t.Fatalf("item1 = %v, want claimed", r.f.Item(item1)["status"])
	}
	r.released(t)
}

// The refused item gone from the ready listing for a tick and listed
// again lifts the hold, the goal row unchanged.
func TestClaimHoldLiftsWhenTheItemIsListedAgain(t *testing.T) {
	r := newClaimHoldRig(t, budgetPaused)
	r.tick(t)
	r.tick(t)
	if n := r.claims(); n != 1 {
		t.Fatalf("claimWorkItem asked %d times, want 1", n)
	}
	r.held(t)

	r.f.WorkItems[item1]["status"] = "pending"
	r.tick(t)
	if n := r.claims(); n != 1 {
		t.Fatalf("claimWorkItem asked %d times with nothing listed, want 1", n)
	}
	r.f.WorkItems[item1]["status"] = "ready"
	delete(r.f.ClaimRefusals, item1)
	r.tick(t)
	if n := r.claims(); n != 2 {
		t.Fatalf("claimWorkItem asked %d times after the item was listed again, want 2", n)
	}
	r.released(t)
}

// A refusal no class names is asked again every tick, as today.
func TestClaimRefusedUnclassedIsNotHeld(t *testing.T) {
	r := newClaimHoldRig(t, "item is claimed, not ready")
	for range 5 {
		r.tick(t)
	}
	if n := r.claims(); n != 5 {
		t.Fatalf("claimWorkItem asked %d times in 5 ticks, want 5", n)
	}
	if h := r.loop.HeldGoals(); len(h) != 0 {
		t.Fatalf("HeldGoals = %+v, want none", h)
	}
	logs := r.logs.String()
	if strings.Contains(logs, "held (") || !strings.Contains(logs, "not ours (item is claimed, not ready)") {
		t.Fatalf("logs:\n%s", logs)
	}
}
