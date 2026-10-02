package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// WorkItemPriorAttempts returns the evaluations already recorded against
// an item, oldest attempt first, with the clauses each one failed on.
func TestWorkItemPriorAttemptsReturnsRecordedAttemptsInOrder(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	evaluatingItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	clauses := []gateway.FailedClause{{Clause: "the shards run within 20% of each other", Satisfy: "a reading recorded honestly under load"}}
	if err := gw.EvaluateWorkItem(ctx, finishItem, false, "first attempt: fails on load", clauses, 10, nil, gateway.EvaluateChoice{}); err != nil {
		t.Fatal(err)
	}
	if err := gw.EvaluateWorkItem(ctx, finishItem, false, "second attempt: fails again", nil, 10, nil, gateway.EvaluateChoice{}); err != nil {
		t.Fatal(err)
	}

	got, err := gw.WorkItemPriorAttempts(ctx, finishItem)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d attempts, want 2: %+v", len(got), got)
	}
	if got[0].Attempt != 1 || got[0].Notes != "first attempt: fails on load" {
		t.Fatalf("attempt 1 = %+v", got[0])
	}
	if len(got[0].Clauses) != 1 || got[0].Clauses[0] != clauses[0] {
		t.Fatalf("attempt 1 clauses = %+v", got[0].Clauses)
	}
	if got[1].Attempt != 2 || got[1].Notes != "second attempt: fails again" {
		t.Fatalf("attempt 2 = %+v", got[1])
	}
	if len(got[1].Clauses) != 0 {
		t.Fatalf("attempt 2 clauses = %+v, want none", got[1].Clauses)
	}
}

// A platform whose WorkItemIndex has no evaluations (an older one, before
// this item) answers the read with an error, which the caller treats as
// none: the evaluation runs without the lineage.
func TestWorkItemPriorAttemptsOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NoEvaluations = true
	evaluatingItem(f)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	if err := gw.EvaluateWorkItem(ctx, finishItem, false, "first attempt", nil, 10, nil, gateway.EvaluateChoice{}); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.WorkItemPriorAttempts(ctx, finishItem); err == nil {
		t.Fatal("a platform without evaluations answered without an error")
	}
}
