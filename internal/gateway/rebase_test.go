package gateway_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const otherNode = "dddddddd-4444-4444-8444-dddddddddddd"

// rebaseItem puts an item in review whose rebase is in the given state,
// as RequestWorkItemRebase leaves it (runsheet ADR 008, trd §6a).
func rebaseItem(f *gatewaytest.Fake, id string, seq int, state string) {
	f.WorkItems[id] = map[string]any{
		"id": id, "orgId": orgID, "seq": seq, "status": "review", "attempts": 2,
		"rebaseState": state, "rebaseConflict": "<<<<<<< main.go", "rebaseFiles": []any{"main.go"},
	}
}

// Only the org's items whose rebase is requested are listed, by seq:
// not one claimed, rebased or never asked for, nor another org's.
func TestRequestedRebases(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	rebaseItem(f, "item-3", 3, "requested")
	rebaseItem(f, "item-1", 1, "requested")
	rebaseItem(f, "item-2", 2, "claimed")
	rebaseItem(f, "item-4", 4, "rebased")
	rebaseItem(f, "item-5", 5, "none")
	rebaseItem(f, "item-6", 6, "requested")
	f.WorkItems["item-6"]["orgId"] = "another-org"
	gw := gateway.New(f.URL(), "rs_test")

	items, err := gw.RequestedRebases(context.Background(), orgID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
		if it.RebaseState != "requested" || it.RebaseConflict == "" || !reflect.DeepEqual(it.RebaseFiles, []string{"main.go"}) {
			t.Fatalf("item %s decoded as %+v", it.ID, it)
		}
	}
	if !reflect.DeepEqual(ids, []string{"item-1", "item-3"}) {
		t.Fatalf("listed %v, want [item-1 item-3]", ids)
	}
	reads := f.ReadsNamed("workItemIndexs")
	if len(reads) != 1 || reads[0].Input["rebase_state"] != "requested" || reads[0].Input["org_id"] != orgID {
		t.Fatalf("reads = %+v", reads)
	}
}

// The claim is the lock: the node that has it converges, a second node
// is refused, so two nodes never both run one rebase.
func TestClaimWorkItemRebaseOnce(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	rebaseItem(f, itemID, 1, "requested")
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	if err := gw.ClaimWorkItemRebase(ctx, itemID, nodeID); err != nil {
		t.Fatal(err)
	}
	if err := gw.ClaimWorkItemRebase(ctx, itemID, nodeID); err != nil {
		t.Fatalf("the holder's second claim: %v", err)
	}
	err := gw.ClaimWorkItemRebase(ctx, itemID, otherNode)
	var gerr *gateway.Error
	if !errors.As(err, &gerr) {
		t.Fatalf("a second node's claim = %v, want a gateway error", err)
	}
	row := f.Item(itemID)
	if row["rebaseState"] != "claimed" || row["rebaseNodeId"] != nodeID {
		t.Fatalf("item = %+v", row)
	}
	calls := f.CallsNamed("claimWorkItemRebase")
	if len(calls) != 2 || calls[0].Input["aggregateId"] != itemID || calls[0].Input["nodeId"] != nodeID {
		t.Fatalf("calls = %+v", calls)
	}
}

// Nothing is claimable that was not requested.
func TestClaimWorkItemRebaseNotRequested(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	rebaseItem(f, itemID, 1, "none")
	gw := gateway.New(f.URL(), "rs_test")

	var gerr *gateway.Error
	if err := gw.ClaimWorkItemRebase(context.Background(), itemID, nodeID); !errors.As(err, &gerr) {
		t.Fatalf("claim = %v, want a gateway error", err)
	}
}

// A finished rebase records the pushed head and the files it resolved,
// method session; the item stays in review with its attempts.
func TestFinishWorkItemRebase(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	rebaseItem(f, itemID, 1, "claimed")
	f.WorkItems[itemID]["rebaseNodeId"] = nodeID
	gw := gateway.New(f.URL(), "rs_test")

	if err := gw.FinishWorkItemRebase(context.Background(), itemID, "abc123", []string{"main.go", "go.sum"}, nil); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("finishWorkItemRebase")
	if len(calls) != 1 || calls[0].Input["headSha"] != "abc123" || calls[0].Input["method"] != "session" ||
		!reflect.DeepEqual(calls[0].Input["files"], []any{"main.go", "go.sum"}) {
		t.Fatalf("calls = %+v", calls)
	}
	it, err := gw.WorkItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.RebaseState != "rebased" || it.RebasedHeadSHA != "abc123" || !reflect.DeepEqual(it.RebaseFiles, []string{"main.go", "go.sum"}) ||
		it.Status != "review" || it.Attempts != 2 {
		t.Fatalf("item = %+v", it)
	}
}

// A rebase given up records the reason and hands the item to its
// builder: ready, the reason its last error, attempts unchanged, the
// rebase over. Said again, it converges.
func TestFailWorkItemRebase(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	rebaseItem(f, itemID, 1, "claimed")
	f.WorkItems[itemID]["rebaseNodeId"] = nodeID
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	const reason = "main.go: both sides rewrote Run; the intent conflicts"
	if err := gw.FailWorkItemRebase(ctx, itemID, reason); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("failWorkItemRebase")
	if len(calls) != 1 || calls[0].Input["reason"] != reason {
		t.Fatalf("calls = %+v", calls)
	}
	it, err := gw.WorkItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.RebaseConflict != reason || it.LastError != reason || it.Status != "ready" || it.Attempts != 2 || it.RebaseState != "none" {
		t.Fatalf("item = %+v", it)
	}
	if err := gw.FailWorkItemRebase(ctx, itemID, reason); err != nil {
		t.Fatalf("a second fail: %v", err)
	}
}
