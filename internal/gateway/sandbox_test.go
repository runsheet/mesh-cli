package gateway_test

import (
	"context"
	"reflect"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// The heartbeat carries `sandbox {mode, runtime, image}` and a limit
// kill's `reason` when set, and neither key when not, so a platform
// without the fields still takes an older node's heartbeat.
func TestHeartbeatSandbox(t *testing.T) {
	f, gw := registeredFake(t)
	ctx := context.Background()
	sb := &gateway.SandboxInfo{Mode: "on", Runtime: "podman 5.2.1", Image: "ghcr.io/projectmesh-io/mesh-node:v1"}
	if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, gateway.HeartbeatExtras{Sandbox: sb, Reason: "sandbox limit: item x/1 killed by its memory limit (6g)"}); err != nil {
		t.Fatal(err)
	}
	if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, gateway.HeartbeatExtras{Sandbox: &gateway.SandboxInfo{Mode: "off"}}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 2 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	want := map[string]any{"mode": "on", "runtime": "podman 5.2.1", "image": "ghcr.io/projectmesh-io/mesh-node:v1"}
	if got := hb[0].Input["sandbox"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("sandbox = %#v", got)
	}
	if got := hb[0].Input["reason"]; got != "sandbox limit: item x/1 killed by its memory limit (6g)" {
		t.Fatalf("reason = %#v", got)
	}
	if got := hb[1].Input["sandbox"]; !reflect.DeepEqual(got, map[string]any{"mode": "off"}) {
		t.Fatalf("off = %#v", got)
	}
	if _, ok := hb[1].Input["reason"]; ok {
		t.Fatalf("no reason, no key: %#v", hb[1].Input)
	}
	for _, x := range [][]gateway.HeartbeatExtras{nil, {{}}, {{Draining: true}}} {
		if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 0, x...); err != nil {
			t.Fatal(err)
		}
	}
	for i, hb := range f.CallsNamed("heartbeatNode")[2:] {
		for _, k := range []string{"sandbox", "reason"} {
			if v, ok := hb.Input[k]; ok {
				t.Fatalf("heartbeat %d sent %s = %#v", i+2, k, v)
			}
		}
	}
}
