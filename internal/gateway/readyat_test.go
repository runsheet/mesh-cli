package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A ready item read from a platform that serves readyAt carries it: the
// claim order's tiebreak within a class (ADR-017).
func TestReadyWorkItemsCarryReadyAt(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.WorkItems["item-1"] = map[string]any{"id": "item-1", "orgId": orgID, "seq": 1, "status": "ready", "readyAt": "2026-09-28T02:24:00Z"}
	gw := gateway.New(f.URL(), "rs_test")
	items, err := gw.ReadyWorkItems(context.Background(), orgID)
	if err != nil || len(items) != 1 || items[0].ReadyAt != "2026-09-28T02:24:00Z" {
		t.Fatalf("ready = %+v, %v", items, err)
	}
}

// A platform older than readyAt refuses the selection: the ready items
// still list, with no readyAt, and the field is not asked for again.
func TestReadyWorkItemsWithoutReadyAtOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields["readyAt"] = true
	f.WorkItems["item-1"] = map[string]any{"id": "item-1", "orgId": orgID, "seq": 1, "status": "ready", "readyAt": "2026-09-28T02:24:00Z"}
	gw := gateway.New(f.URL(), "rs_test")
	for i := 0; i < 2; i++ {
		items, err := gw.ReadyWorkItems(context.Background(), orgID)
		if err != nil || len(items) != 1 || items[0].ID != "item-1" || items[0].ReadyAt != "" {
			t.Fatalf("read %d: %+v, %v", i, items, err)
		}
	}
	if n := len(f.ReadsNamed("workItemIndexs")); n != 3 {
		t.Fatalf("workItemIndexs read %d times, want 3", n)
	}
}
