package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A release is the node's condition, not the item's: the item goes back
// to ready with the claim let go, its attempts and its last error as they
// were, and the reason in its last note.
func TestReleaseWorkItem(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.WorkItems["i1"] = map[string]any{
		"id": "i1", "orgId": orgID, "status": "claimed", "nodeId": nodeID,
		"harness": "claude-code", "attempts": 1, "lastError": "an earlier verdict",
	}
	gw := gateway.New(f.URL(), "rs_test")

	reason := "You've hit your monthly spend limit"
	if err := gw.ReleaseWorkItem(context.Background(), "i1", reason); err != nil {
		t.Fatal(err)
	}
	item := f.Item("i1")
	if item["status"] != "ready" {
		t.Errorf("status = %v, want ready", item["status"])
	}
	if v, ok := item["nodeId"]; ok && v != nil && v != "" {
		t.Errorf("nodeId = %v, want cleared", v)
	}
	if v, ok := item["harness"]; ok && v != nil && v != "" {
		t.Errorf("harness = %v, want cleared", v)
	}
	if item["lastError"] != "an earlier verdict" {
		t.Errorf("lastError = %v, want untouched", item["lastError"])
	}
	if item["attempts"] != 1 {
		t.Errorf("attempts = %v, want unchanged", item["attempts"])
	}
	if item["lastNote"] != reason {
		t.Errorf("lastNote = %v, want the reason", item["lastNote"])
	}
}

// The heartbeat carries the node's status and why; an empty status is
// the ordinary heartbeat and reads as online again.
func TestHeartbeatNodeStatus(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	if err := gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: nodeID, OrgID: orgID, Name: "mini", Capacity: 1}); err != nil {
		t.Fatal(err)
	}

	reason := "You've hit your monthly spend limit · raise it at claude.ai/settings/usage"
	if err := gw.HeartbeatNode(ctx, nodeID, "v1", 0, "harness_unavailable", reason); err != nil {
		t.Fatal(err)
	}
	if n := f.Nodes[nodeID]; n["status"] != "harness_unavailable" || n["reason"] != reason {
		t.Fatalf("node after harness_unavailable: %+v", n)
	}

	if err := gw.HeartbeatNode(ctx, nodeID, "v1", 0, "", ""); err != nil {
		t.Fatal(err)
	}
	if n := f.Nodes[nodeID]; n["status"] != "online" || n["reason"] != "" {
		t.Fatalf("node after an ordinary heartbeat: %+v", n)
	}
}
