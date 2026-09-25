package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A ranged node's heartbeat carries scaling: the capacity in force, the
// range and the reason (adr-004).
func TestHeartbeatCarriesScaling(t *testing.T) {
	f, gw := registeredFake(t)
	err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 2, gateway.HeartbeatExtras{
		Scaling: &gateway.Scaling{Capacity: 3, Min: 1, Max: 4, Reason: "weekly pace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	s, ok := hb[0].Input["scaling"].(map[string]any)
	if !ok {
		t.Fatalf("scaling = %#v", hb[0].Input["scaling"])
	}
	for k, want := range map[string]any{"capacity": 3.0, "min": 1.0, "max": 4.0, "reason": "weekly pace"} {
		if s[k] != want {
			t.Fatalf("scaling.%s = %#v, want %#v", k, s[k], want)
		}
	}
	if len(s) != 4 {
		t.Fatalf("scaling = %#v, want the four fields only", s)
	}
}

// A heartbeat without scaling has no scaling key, so a platform that
// does not know the field still takes a fixed node's heartbeat.
func TestHeartbeatWithoutScalingOmitsIt(t *testing.T) {
	f, gw := registeredFake(t)
	ctx := context.Background()
	for _, x := range [][]gateway.HeartbeatExtras{nil, {{}}, {{Draining: true}}} {
		if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 0, x...); err != nil {
			t.Fatal(err)
		}
	}
	for i, hb := range f.CallsNamed("heartbeatNode") {
		if v, ok := hb.Input["scaling"]; ok {
			t.Fatalf("heartbeat %d sent scaling = %#v", i, v)
		}
	}
}
