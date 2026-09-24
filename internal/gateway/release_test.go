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
