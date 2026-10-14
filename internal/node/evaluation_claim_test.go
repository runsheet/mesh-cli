package node

import (
	"context"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// An item's evaluation runs on one node (ADR-025's amendment
// 2026-10-14): the builder claims its own item's evaluation too, so the
// platform's claim is the one lock across nodes.

// ownEvaluationFake is a priorityFake with one item in evaluating that
// this node built, held-3 of globex's goal held.
func ownEvaluationFake(t *testing.T) *priorityFake {
	p := newPriorityFake(t)
	p.addGoal(org2, "held", "normal")
	p.addHeld(org2, "held", "held-3", 3)
	return p
}

// ownEvaluationRun runs ticks of a capacity-2 loop over p and reports
// its log, the items Evaluate was started on, and how many
// claimWorkItemEvaluation calls had been sent when each started.
func ownEvaluationRun(t *testing.T, p *priorityFake, ticks int) (string, []gateway.WorkItem, []int) {
	t.Helper()
	loop, logs := p.loop(2)
	var mu sync.Mutex
	var evaluated []gateway.WorkItem
	var claimsBefore []int
	loop.Evaluate = func(_ context.Context, it gateway.WorkItem) {
		mu.Lock()
		defer mu.Unlock()
		evaluated = append(evaluated, it)
		claimsBefore = append(claimsBefore, len(p.CallsNamed("claimWorkItemEvaluation")))
	}
	for range ticks {
		loop.Tick(context.Background())
		loop.wg.Wait()
	}
	return logs.String(), evaluated, claimsBefore
}

// (a) A node evaluating an item it built claims it with
// ClaimWorkItemEvaluation before it logs `evaluating item`.
func TestBuilderClaimsItsOwnEvaluation(t *testing.T) {
	p := ownEvaluationFake(t)
	logs, evaluated, claimsBefore := ownEvaluationRun(t, p, 1)
	calls := p.CallsNamed("claimWorkItemEvaluation")
	if len(calls) != 1 {
		t.Fatalf("%d claimWorkItemEvaluation call(s), want 1:\n%s", len(calls), logs)
	}
	if c := calls[0]; c.Input["aggregateId"] != "held-3" || c.Input["nodeId"] != p.scopes[1].NodeID {
		t.Errorf("claimWorkItemEvaluation input = %v, want held-3 by %s", c.Input, p.scopes[1].NodeID)
	}
	if len(evaluated) != 1 || evaluated[0].ID != "held-3" {
		t.Fatalf("evaluated = %+v, want held-3", evaluated)
	}
	if claimsBefore[0] != 1 {
		t.Errorf("Evaluate started with %d claim(s) sent, want the claim first", claimsBefore[0])
	}
	claimed := strings.Index(logs, "claimed the evaluation of item 3")
	started := strings.Index(logs, `evaluating item 3 "held-3"`)
	if claimed < 0 || started < 0 || claimed > started {
		t.Errorf("want the claim logged before `evaluating item`:\n%s", logs)
	}
	if got := p.WorkItems["held-3"]["evaluationNodeId"]; got != p.scopes[1].NodeID {
		t.Errorf("evaluationNodeId = %v, want the builder %s", got, p.scopes[1].NodeID)
	}
}

// (b) The builder's claim refused — another node took the evaluation —
// logs no `evaluating item`, starts no Evaluate, leaves the item out of
// l.evaluating and says whose it is once per item, not as an error.
func TestBuilderRefusedItsOwnEvaluationEvaluatesNothing(t *testing.T) {
	p := ownEvaluationFake(t)
	p.WorkItems["held-3"]["evaluationNodeId"] = otherNode
	loop, logs := p.loop(2)
	var mu sync.Mutex
	var evaluated []gateway.WorkItem
	loop.Evaluate = func(_ context.Context, it gateway.WorkItem) {
		mu.Lock()
		evaluated = append(evaluated, it)
		mu.Unlock()
	}
	for range 2 {
		loop.Tick(context.Background())
		loop.wg.Wait()
	}
	out := logs.String()
	if len(p.Claims) != 2 || p.Claims[0].Name != "claimWorkItemEvaluation" {
		t.Fatalf("claims attempted = %+v, want claimWorkItemEvaluation each tick", p.Claims)
	}
	if len(evaluated) != 0 {
		t.Errorf("Evaluate started on %+v after a refused claim", evaluated)
	}
	if strings.Contains(out, "evaluating item") {
		t.Errorf("`evaluating item` logged after a refused claim:\n%s", out)
	}
	loop.mu.Lock()
	left := loop.evaluating["held-3"]
	loop.mu.Unlock()
	if left {
		t.Error("held-3 left in the loop's evaluating set after a refused claim")
	}
	want := "item held/3: evaluation is node " + otherNode + "'s; not evaluated here"
	if n := strings.Count(out, want); n != 1 {
		t.Errorf("%q logged %d times over two ticks, want once:\n%s", want, n, out)
	}
	if strings.Contains(out, "claim the evaluation") {
		t.Errorf("a refusal logged as an error:\n%s", out)
	}
}

// (c) A gateway without claimWorkItemEvaluation: the node's own item is
// evaluated with no claim, as before.
func TestNoEvaluationClaimEvaluatesOwnWithoutClaim(t *testing.T) {
	p := ownEvaluationFake(t)
	p.Unsupported = map[string]bool{"claimWorkItemEvaluation": true}
	logs, evaluated, _ := ownEvaluationRun(t, p, 1)
	if n := len(p.Claims); n != 0 {
		t.Errorf("claims attempted = %+v on a platform without the claim", p.Claims)
	}
	if len(evaluated) != 1 || evaluated[0].ID != "held-3" {
		t.Fatalf("evaluated = %+v, want held-3:\n%s", evaluated, logs)
	}
	if strings.Contains(logs, "claimed the evaluation") {
		t.Errorf("a claim logged on a platform without it:\n%s", logs)
	}
}
