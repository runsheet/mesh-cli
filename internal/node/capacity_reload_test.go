package node

import (
	"context"
	"testing"
)

// The mini's node.yaml went from `capacity: 6` to `capacity: 8` under a
// running node (goal node-yaml-takes-effect-or-says-why-not): the loop
// applied it and logged it, but the platform still said 6, because a
// fixed node on one account heartbeated no capacity at all. Driven the
// whole way, file on disk through Reloader to the heartbeat the gateway
// receives: the next tick runs at 8, logs the change as one whole line,
// and its heartbeat carries 8.
func TestCapacityReloadFromNodeYAMLReachesTheHeartbeat(t *testing.T) {
	p := newPriorityFake(t)
	p.register()
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 6\n")

	loop.Tick(context.Background())
	loop.wg.Wait()
	if got := loop.Capacity(); got != 6 {
		t.Fatalf("first tick: capacity %d, want 6", got)
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 8\n")
	before := len(p.CallsNamed("heartbeatNode"))
	loop.Tick(context.Background())
	loop.wg.Wait()

	if got := loop.Capacity(); got != 8 {
		t.Fatalf("after node.yaml capacity 6 → 8: capacity %d, want 8", got)
	}
	if !hasLine(logs.String(), "capacity 6 → 8") {
		t.Fatalf("the change is not logged as the line %q:\n%s", "capacity 6 → 8", logs)
	}
	hb := p.CallsNamed("heartbeatNode")[before:]
	if len(hb) == 0 {
		t.Fatalf("the second tick sent no heartbeat:\n%s", logs)
	}
	s, ok := hb[len(hb)-1].Input["scaling"].(map[string]any)
	if !ok || s["capacity"] != 8.0 {
		t.Fatalf("the heartbeat after the reload carries scaling %#v, want capacity 8", hb[len(hb)-1].Input["scaling"])
	}
}
