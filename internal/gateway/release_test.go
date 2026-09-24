package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A released item is ready again with its node cleared, the reason its
// last note and not its last error, attempts unchanged (trd §1).
func TestReleaseWorkItem(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NodeReleases = true
	f.WorkItems[itemID] = map[string]any{"id": itemID, "orgId": orgID, "seq": 1, "status": "running", "nodeId": nodeID, "attempts": 1}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	const reason = "github credential: acme/widgets token expiring 2036-09-24T11:11:55Z refused 4 times: Repository not found"
	if err := gw.ReleaseWorkItem(ctx, itemID, nodeID, reason); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("releaseWorkItem")
	if len(calls) != 1 || calls[0].Input["nodeId"] != nodeID || calls[0].Input["note"] != reason {
		t.Fatalf("calls = %+v", calls)
	}
	it, err := gw.WorkItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "ready" || it.NodeID != "" || it.LastNote != reason || it.LastError != "" || it.Attempts != 1 {
		t.Fatalf("item = %+v", it)
	}
}

// The deployed platform refuses a node's release: the item is left as it
// was and the node is told so.
func TestReleaseWorkItemRefusedByTheDeployedPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.WorkItems[itemID] = map[string]any{"id": itemID, "orgId": orgID, "seq": 1, "status": "running", "nodeId": nodeID, "attempts": 1}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()
	if err := gw.ReleaseWorkItem(ctx, itemID, nodeID, "github credential: acme/widgets"); err == nil {
		t.Fatal("a node's release must be refused")
	}
	it, err := gw.WorkItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "running" || it.NodeID != nodeID || it.LastNote != "" {
		t.Fatalf("item = %+v", it)
	}
}

// Once the platform lets a node release, it releases only a claimed or running item, and only for the
// node that holds it; any other release changes nothing.
func TestReleaseWorkItemIgnoredUnlessHeldAndRunning(t *testing.T) {
	const other = "33333333-3333-4333-8333-333333333333"
	for _, tc := range []struct{ name, status, holder string }{
		{"evaluating", "evaluating", nodeID},
		{"another node's", "running", other},
		{"ready", "ready", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gatewaytest.New("rs_test")
			defer f.Close()
			f.NodeReleases = true
			row := map[string]any{"id": itemID, "orgId": orgID, "seq": 1, "status": tc.status, "attempts": 1}
			if tc.holder != "" {
				row["nodeId"] = tc.holder
			}
			f.WorkItems[itemID] = row
			gw := gateway.New(f.URL(), "rs_test")
			ctx := context.Background()
			if err := gw.ReleaseWorkItem(ctx, itemID, nodeID, "github credential: acme/widgets"); err != nil {
				t.Fatal(err)
			}
			it, err := gw.WorkItem(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			if it.Status != tc.status || it.NodeID != tc.holder || it.LastNote != "" || it.Attempts != 1 {
				t.Fatalf("item = %+v", it)
			}
		})
	}
}
