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
	want := []any{map[string]any{"itemId": "item-1", "kind": "build", "since": "2026-09-25T09:48:00Z", "alive": false}}
	if got := hb[0].Input["adopted"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("adopted = %#v", got)
	}
	for i, h := range hb[1:] {
		if v, ok := h.Input["adopted"]; ok {
			t.Fatalf("heartbeat %d sent adopted = %#v", i+1, v)
		}
	}
}

// A gateway whose `adopted` element type takes goalId, directionId,
// sessionId and alive gets a heartbeat whose plan entry carries itemId
// and goalId equal to the goal id, kind plan, since, sessionId and
// alive, and whose research entry carries directionId (mesh-cli
// ADR-009 amendment).
func TestHeartbeatAdoptedCarriesGoalDirectionSessionAndAlive(t *testing.T) {
	f, gw := registeredFake(t)
	ctx := context.Background()
	since := time.Date(2026, 9, 25, 9, 48, 0, 0, time.UTC)
	x := gateway.HeartbeatExtras{Adopted: []gateway.AdoptedSession{
		{ItemID: "goal-1", GoalID: "goal-1", Kind: "plan", Since: since, SessionID: "sess-1", Alive: true},
		{ItemID: "direction-1", DirectionID: "direction-1", Kind: "research", Since: since, SessionID: "sess-2", Alive: false},
	}}
	if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, x); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	adopted, ok := hb[0].Input["adopted"].([]any)
	if !ok || len(adopted) != 2 {
		t.Fatalf("adopted = %#v", hb[0].Input["adopted"])
	}
	plan, ok := adopted[0].(map[string]any)
	if !ok {
		t.Fatalf("adopted[0] = %#v", adopted[0])
	}
	if plan["itemId"] != "goal-1" || plan["goalId"] != "goal-1" || plan["kind"] != "plan" || plan["since"] == "" || plan["sessionId"] != "sess-1" || plan["alive"] != true {
		t.Fatalf("plan entry = %#v", plan)
	}
	research, ok := adopted[1].(map[string]any)
	if !ok {
		t.Fatalf("adopted[1] = %#v", adopted[1])
	}
	if research["directionId"] != "direction-1" || research["sessionId"] != "sess-2" || research["alive"] != false {
		t.Fatalf("research entry = %#v", research)
	}
}

// A gateway whose `adopted` element type lacks goalId, directionId,
// sessionId and alive gets a heartbeat with only itemId, kind and
// since, and no error — the platform predates the amendment.
func TestHeartbeatOmitsAdoptedFieldsTheSchemaLacks(t *testing.T) {
	f, gw := registeredFake(t)
	f.NoAdoptedFields = true
	ctx := context.Background()
	since := time.Date(2026, 9, 25, 9, 48, 0, 0, time.UTC)
	x := gateway.HeartbeatExtras{Adopted: []gateway.AdoptedSession{
		{ItemID: "goal-1", GoalID: "goal-1", Kind: "plan", Since: since, SessionID: "sess-1", Alive: true},
	}}
	if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, x); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	adopted, ok := hb[0].Input["adopted"].([]any)
	if !ok || len(adopted) != 1 {
		t.Fatalf("adopted = %#v", hb[0].Input["adopted"])
	}
	entry, ok := adopted[0].(map[string]any)
	if !ok {
		t.Fatalf("adopted[0] = %#v", adopted[0])
	}
	want := map[string]any{"itemId": "goal-1", "kind": "plan", "since": "2026-09-25T09:48:00Z"}
	if !reflect.DeepEqual(entry, want) {
		t.Fatalf("entry = %#v, want %#v", entry, want)
	}

	// the platform deploys the fields; the next probe (after
	// ScalingProbeEvery) sees them
	f.NoAdoptedFields = false
	gw.Now = func() time.Time { return time.Now().Add(gateway.ScalingProbeEvery) }
	if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, x); err != nil {
		t.Fatal(err)
	}
	hb = f.CallsNamed("heartbeatNode")
	adopted, ok = hb[len(hb)-1].Input["adopted"].([]any)
	if !ok || len(adopted) != 1 {
		t.Fatalf("adopted = %#v", hb[len(hb)-1].Input["adopted"])
	}
	entry = adopted[0].(map[string]any)
	if entry["goalId"] != "goal-1" || entry["sessionId"] != "sess-1" || entry["alive"] != true {
		t.Fatalf("entry = %#v, want the new fields once the schema has them", entry)
	}
}
