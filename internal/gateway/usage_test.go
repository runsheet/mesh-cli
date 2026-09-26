package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A priced tool's usage record goes in as one input, and the fake keeps it.
func TestRecordUsage(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	if err := gw.RecordUsage(context.Background(), gateway.ToolUsage{ItemID: itemID, Kind: "build", Tool: "search", Calls: 2, ListPriceCents: 6}); err != nil {
		t.Fatal(err)
	}
	if len(f.Usage) != 1 {
		t.Fatalf("usage = %v", f.Usage)
	}
	in := f.Usage[0]
	if in["workItemId"] != itemID || in["kind"] != "build" || in["tool"] != "search" || in["calls"] != 2.0 || in["listPriceCents"] != 6.0 || in["namespace"] != "runsheet" {
		t.Fatalf("input = %v", in)
	}
}

// An older platform's unknown-mutation error is IsUnsupported: the node's
// cue to carry on, not to fail the item.
func TestRecordUsageUnsupported(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Unsupported["recordUsage"] = true
	gw := gateway.New(f.URL(), "rs_test")
	if err := gw.RecordUsage(context.Background(), gateway.ToolUsage{ItemID: itemID, Kind: "build", Tool: "search", Calls: 1, ListPriceCents: 3}); !gateway.IsUnsupported(err) {
		t.Fatalf("RecordUsage = %v, want unsupported", err)
	}
	if len(f.Usage) != 0 {
		t.Fatalf("usage = %v", f.Usage)
	}
}
