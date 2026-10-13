package gateway_test

import (
	"context"
	"reflect"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A gateway whose HeartbeatNodeInput has `resumed` gets each resumed
// build session as {itemId, kind, sessionId, fresh}.
func TestHeartbeatResumed(t *testing.T) {
	f, gw := registeredFake(t)
	ctx := context.Background()
	x := gateway.HeartbeatExtras{Resumed: []gateway.ResumedSession{{ItemID: "item-1", Kind: "build", SessionID: "sess-1", Fresh: true}}}
	if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, x); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, gateway.HeartbeatExtras{}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 2 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	want := []any{map[string]any{"itemId": "item-1", "kind": "build", "sessionId": "sess-1", "fresh": true}}
	if got := hb[0].Input["resumed"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("resumed = %#v, want %#v", got, want)
	}
	if v, ok := hb[1].Input["resumed"]; ok {
		t.Fatalf("a heartbeat with nothing resumed sent resumed = %#v", v)
	}
}

// A gateway whose HeartbeatNodeInput lacks `resumed` gets the heartbeat
// without the key, and no error: the platform predates the field.
func TestHeartbeatOmitsResumedTheSchemaLacks(t *testing.T) {
	f, gw := registeredFake(t)
	f.UnknownFields["resumed"] = true
	ctx := context.Background()
	x := gateway.HeartbeatExtras{Resumed: []gateway.ResumedSession{{ItemID: "item-1", Kind: "build", SessionID: "sess-1"}}}
	if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, x); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	if v, ok := hb[0].Input["resumed"]; ok {
		t.Fatalf("resumed = %#v sent to a schema without it", v)
	}
}
