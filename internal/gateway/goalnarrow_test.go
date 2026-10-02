package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A narrow re-plan's goal carries replacingItemId and narrowReplans
// (runsheet ADR 023): Goal reads them.
func TestGoalReadsReplacingItemID(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Goals["g1"] = map[string]any{"id": "g1", "status": "planning", "replacingItemId": "item-2", "narrowReplans": 1}
	gw := gateway.New(f.URL(), "rs_test")

	g, err := gw.Goal(context.Background(), "g1")
	if err != nil {
		t.Fatal(err)
	}
	if g.ReplacingItemID != "item-2" || g.NarrowReplans != 1 {
		t.Fatalf("goal = %+v", g)
	}
}

// A platform older than ADR 023 refuses them: the read falls back to the
// required fields — every plan whole — and stops asking for them.
func TestGoalWithoutReplacingItemIDOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields["replacingItemId"] = true
	f.Goals["g1"] = map[string]any{"id": "g1", "status": "planning"}
	gw := gateway.New(f.URL(), "rs_test")

	for i := 0; i < 2; i++ {
		g, err := gw.Goal(context.Background(), "g1")
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if g.ID != "g1" || g.ReplacingItemID != "" {
			t.Fatalf("read %d: %+v", i, g)
		}
	}
	if n := len(f.ReadsNamed("goalIndex")); n != 3 {
		t.Fatalf("goalIndex read %d times, want 3", n)
	}
}
