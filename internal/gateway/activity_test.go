package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// ItemActivity reads the item's own rows, oldest first whatever order
// the gateway answers in, and none of another item's.
func TestItemActivityReadsTheItemsRowsInTimeOrder(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Activity = []map[string]any{
		{"id": "a2", "workItemId": "item-1", "kind": "progress", "summary": "second", "at": "2026-10-17T10:02:00Z", "logSeq": 2},
		{"id": "a1", "workItemId": "item-1", "kind": "progress", "summary": "first", "at": "2026-10-17T10:01:00Z", "logSeq": 1},
		{"id": "a3", "workItemId": "item-1", "kind": "progress", "summary": "question: which?", "at": "2026-10-17T10:02:00Z", "logSeq": 3},
		{"id": "b1", "workItemId": "item-2", "kind": "progress", "summary": "theirs", "at": "2026-10-17T10:00:00Z", "logSeq": 4},
	}
	got, err := gateway.New(f.URL(), "rs_test").ItemActivity(context.Background(), "item-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Summary != "first" || got[1].Summary != "second" || got[2].Summary != "question: which?" {
		t.Fatalf("activity %+v", got)
	}
}

// A platform with no meshActivityIndexs is Unsupported.
func TestItemActivityUnsupported(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.FailReads["meshActivityIndexs"] = `Cannot query field "meshActivityIndexs" on type "Query".`
	_, err := gateway.New(f.URL(), "rs_test").ItemActivity(context.Background(), "item-1")
	if !gateway.IsUnsupported(err) {
		t.Fatalf("err %v, want unsupported", err)
	}
}
