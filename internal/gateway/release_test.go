package gateway_test

import (
	"context"
	"reflect"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const releaseReason = "github credential: acme/widgets token expiring 2036-09-24T11:11:55Z refused 4 times: Repository not found"

// A released item is ready again with its node, harness and session
// cleared, attempts unchanged, the reason its last note and its last
// error untouched (trd §1).
func TestReleaseWorkItem(t *testing.T) {
	for _, status := range []string{"claimed", "running"} {
		t.Run(status, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			defer f.Close()
			f.TokenNodeID = nodeID
			f.WorkItems[itemID] = map[string]any{
				"id": itemID, "orgId": orgID, "seq": 1, "status": status, "nodeId": nodeID,
				"harness": "claude", "sessionId": "sess-1", "attempts": 1, "lastError": "an earlier evaluation",
			}
			gw := gateway.New(f.URL(), "rs_test")
			ctx := context.Background()

			if err := gw.ReleaseWorkItem(ctx, itemID, nodeID, releaseReason); err != nil {
				t.Fatal(err)
			}
			calls := f.CallsNamed("releaseWorkItem")
			if len(calls) != 1 || calls[0].Input["aggregateId"] != itemID ||
				calls[0].Input["nodeId"] != nodeID || calls[0].Input["note"] != releaseReason {
				t.Fatalf("calls = %+v", calls)
			}
			it, err := gw.WorkItem(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			if it.Status != "ready" || it.NodeID != "" || it.Attempts != 1 || it.LastNote != releaseReason || it.LastError != "an earlier evaluation" {
				t.Fatalf("item = %+v", it)
			}
			row := f.Item(itemID)
			if row["harness"] != nil || row["sessionId"] != nil {
				t.Fatalf("harness/session not cleared: %+v", row)
			}
		})
	}
}

// A release of an item another node holds, or of one not claimed or
// running, is refused and changes nothing.
func TestReleaseWorkItemRefusedUnlessHeldAndRunning(t *testing.T) {
	const other = "33333333-3333-4333-8333-333333333333"
	for _, tc := range []struct{ name, status, holder string }{
		{"another node's", "running", other},
		{"another node's claim", "claimed", other},
		{"evaluating", "evaluating", nodeID},
		{"ready", "ready", ""},
		{"failed", "failed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			defer f.Close()
			f.TokenNodeID = nodeID
			row := map[string]any{"id": itemID, "orgId": orgID, "seq": 1, "status": tc.status, "attempts": 1}
			if tc.holder != "" {
				row["nodeId"] = tc.holder
			}
			f.WorkItems[itemID] = row
			before := f.Item(itemID)
			gw := gateway.New(f.URL(), "rs_test")

			if err := gw.ReleaseWorkItem(context.Background(), itemID, nodeID, releaseReason); err == nil {
				t.Fatal("release accepted")
			}
			if after := f.Item(itemID); !reflect.DeepEqual(after, before) {
				t.Fatalf("item changed: %+v, was %+v", after, before)
			}
			if calls := f.CallsNamed("releaseWorkItem"); len(calls) != 0 {
				t.Fatalf("calls = %+v", calls)
			}
		})
	}
}

// A platform that refuses a node's release: the node is told, and the item
// is left as it was.
func TestReleaseWorkItemRefusedByThePlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.TokenNodeID = nodeID
	f.RefuseReleases = true
	f.WorkItems[itemID] = map[string]any{"id": itemID, "orgId": orgID, "seq": 1, "status": "running", "nodeId": nodeID, "attempts": 1}
	before := f.Item(itemID)
	gw := gateway.New(f.URL(), "rs_test")

	if err := gw.ReleaseWorkItem(context.Background(), itemID, nodeID, releaseReason); err == nil {
		t.Fatal("release accepted")
	}
	if after := f.Item(itemID); !reflect.DeepEqual(after, before) {
		t.Fatalf("item changed: %+v, was %+v", after, before)
	}
}

// The heartbeat carries the node's harness_unavailable status and why
// (trd §1 Node); an ordinary heartbeat after it sends no status and
// reads as online again.
func TestHeartbeatNodeStatus(t *testing.T) {
	f, gw := registeredFake(t)
	ctx := context.Background()
	reason := "You've hit your monthly spend limit · raise it at claude.ai/settings/usage"
	if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 0, gateway.HeartbeatExtras{Status: "harness_unavailable", Reason: reason}); err != nil {
		t.Fatal(err)
	}
	if n := f.Nodes[limitsNode]; n["status"] != "harness_unavailable" || n["reason"] != reason {
		t.Fatalf("node after harness_unavailable: %+v", n)
	}
	if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 0); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if _, ok := hb[len(hb)-1].Input["status"]; ok {
		t.Fatalf("an ordinary heartbeat sent a status: %#v", hb[len(hb)-1].Input)
	}
	if n := f.Nodes[limitsNode]; n["status"] != "online" {
		t.Fatalf("node after an ordinary heartbeat: %+v", n)
	}
}
