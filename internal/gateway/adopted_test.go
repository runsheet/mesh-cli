package gateway_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// The heartbeat carries `adopted [{itemId, kind, since}]` when a
// restarted node adopted sessions, and no key when it adopted none.
func TestHeartbeatAdopted(t *testing.T) {
	f, gw := registeredFake(t)
	ctx := context.Background()
	since := time.Date(2026, 9, 25, 9, 48, 0, 0, time.UTC)
	x := gateway.HeartbeatExtras{Adopted: []gateway.AdoptedSession{{ItemID: "item-1", Kind: "build", Since: since}}}
	if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, x); err != nil {
		t.Fatal(err)
	}
	for _, x := range [][]gateway.HeartbeatExtras{nil, {{}}, {{Draining: true, Adopted: []gateway.AdoptedSession{}}}} {
		if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 0, x...); err != nil {
			t.Fatal(err)
		}
	}
	hb := f.CallsNamed("heartbeatNode")
	want := []any{map[string]any{"itemId": "item-1", "kind": "build", "since": "2026-09-25T09:48:00Z"}}
	if got := hb[0].Input["adopted"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("adopted = %#v", got)
	}
	for i, h := range hb[1:] {
		if v, ok := h.Input["adopted"]; ok {
			t.Fatalf("heartbeat %d sent adopted = %#v", i+1, v)
		}
	}
}
