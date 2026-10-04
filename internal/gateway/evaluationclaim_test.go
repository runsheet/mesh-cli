package gateway_test

import (
	"context"
	"errors"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// claimableEvaluation is an item that has finished building and is
// waiting for a node to evaluate it, held by no one (goal
// a-node-says-which-sessions-it-takes: the claim a node that did not
// build the item makes to evaluate it).
func claimableEvaluation(f *gatewaytest.Fake, status string) {
	f.WorkItems[itemID] = map[string]any{"id": itemID, "orgId": orgID, "seq": 1, "status": status, "nodeId": nodeID}
}

// The claim is the lock: a node that did not build the item takes its
// evaluation, recorded like claimWorkItem, and the item's nodeId moves
// to the claimant.
func TestClaimWorkItemEvaluationOnce(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	claimableEvaluation(f, "evaluating")
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	if err := gw.ClaimWorkItemEvaluation(ctx, itemID, otherNode); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("claimWorkItemEvaluation")
	if len(calls) != 1 || calls[0].Input["aggregateId"] != itemID || calls[0].Input["nodeId"] != otherNode {
		t.Fatalf("calls = %+v", calls)
	}
	row := f.Item(itemID)
	if row["nodeId"] != otherNode {
		t.Fatalf("item nodeId = %v, want %v", row["nodeId"], otherNode)
	}
}

// A second, different node's claim is refused: the evaluation has one
// holder, and the item's nodeId stays with the first.
func TestClaimWorkItemEvaluationHeldByAnotherNode(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	claimableEvaluation(f, "evaluating")
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	if err := gw.ClaimWorkItemEvaluation(ctx, itemID, otherNode); err != nil {
		t.Fatal(err)
	}
	// the same node asking again converges
	if err := gw.ClaimWorkItemEvaluation(ctx, itemID, otherNode); err != nil {
		t.Fatalf("the holder's second claim: %v", err)
	}
	thirdNode := "33333333-3333-4333-8333-333333333333"
	err := gw.ClaimWorkItemEvaluation(ctx, itemID, thirdNode)
	var gerr *gateway.Error
	if !errors.As(err, &gerr) {
		t.Fatalf("a second node's claim = %v, want a gateway error", err)
	}
	if !gateway.IsRefusal(err) {
		t.Fatalf("a second node's claim = %v, want IsRefusal", err)
	}
	row := f.Item(itemID)
	if row["nodeId"] != otherNode {
		t.Fatalf("item nodeId = %v, want unchanged %v", row["nodeId"], otherNode)
	}
}

// An item not in evaluating — still building, in this case — is not
// claimable for evaluation.
func TestClaimWorkItemEvaluationNotEvaluating(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	claimableEvaluation(f, "ready")
	gw := gateway.New(f.URL(), "rs_test")

	err := gw.ClaimWorkItemEvaluation(context.Background(), itemID, otherNode)
	if !gateway.IsRefusal(err) {
		t.Fatalf("claim on a ready item = %v, want IsRefusal", err)
	}
	row := f.Item(itemID)
	if row["nodeId"] != nodeID {
		t.Fatalf("item nodeId = %v, want unchanged %v", row["nodeId"], nodeID)
	}
}
