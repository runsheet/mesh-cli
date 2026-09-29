package gateway_test

import (
	"context"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A platform older than the node refuses the whole heartbeat over one
// field it does not know (2026-09-25: limits and scaling), and a node
// whose heartbeats never land is marked offline while it works. The
// node sends the fields every platform takes instead, and keeps doing
// so without asking again each beat.
func TestHeartbeatFallsBackOnFieldsThePlatformDoesNotKnow(t *testing.T) {
	f, gw := registeredFake(t)
	f.UnknownFields = map[string]bool{"limits": true, "scaling": true}
	ctx := context.Background()
	x := gateway.HeartbeatExtras{Draining: true, Limits: []gateway.Limits{{
		Account: "default", FiveHour: &gateway.LimitWindow{Utilization: 0.29, ResetsAt: time.Now()}, ObservedAt: time.Now(),
	}}}
	for i := 0; i < 2; i++ {
		if _, err := gw.HeartbeatNode(ctx, limitsNode, "v2", 3, x); err != nil {
			t.Fatalf("beat %d: %v", i, err)
		}
	}
	hb := f.CallsNamed("heartbeatNode")
	// the fake records only what it accepted: the fallback beat, then a
	// second beat sent basic from the start
	if len(hb) != 2 {
		t.Fatalf("heartbeats accepted: %d, want 2", len(hb))
	}
	for _, c := range hb {
		if _, ok := c.Input["limits"]; ok {
			t.Fatalf("a basic heartbeat carried limits: %#v", c.Input)
		}
		if c.Input["version"] != "v2" || c.Input["draining"] != true {
			t.Fatalf("basic heartbeat lost its fields: %#v", c.Input)
		}
	}
}

// A platform that takes the fields gets them: nothing falls back.
func TestHeartbeatKeepsFieldsThePlatformKnows(t *testing.T) {
	f, gw := registeredFake(t)
	x := gateway.HeartbeatExtras{Limits: []gateway.Limits{{Account: "default", ObservedAt: time.Now()}}}
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v2", 1, x); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 || hb[0].Input["limits"] == nil {
		t.Fatalf("heartbeats: %#v", hb)
	}
}
