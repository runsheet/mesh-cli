package gateway_test

import (
	"context"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const limitsNode = "eeeeeeee-5555-4555-8555-eeeeeeeeeeee"

func registeredFake(t *testing.T) (*gatewaytest.Fake, *gateway.Client) {
	t.Helper()
	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	gw := gateway.New(f.URL(), "rs_test")
	if err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: limitsNode, OrgID: orgID, Name: "mini", Slug: "mini", Capacity: 2, Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	return f, gw
}

// A heartbeat with limits carries them, one per account, in the
// shape of trd §1: camelCase, times in RFC 3339.
func TestHeartbeatCarriesLimits(t *testing.T) {
	f, gw := registeredFake(t)
	resets := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	week := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	seen := time.Date(2026, 9, 25, 14, 12, 5, 0, time.UTC)
	err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 1, gateway.HeartbeatExtras{Limits: []gateway.Limits{{
		Account: "max-20x", Status: "allowed_warning", RateLimitType: "five_hour", ResetsAt: resets,
		OverageStatus: "rejected", IsUsingOverage: false,
		FiveHour:   &gateway.LimitWindow{Utilization: 0.83, ResetsAt: resets},
		SevenDay:   &gateway.LimitWindow{Utilization: 0.41, ResetsAt: week},
		ObservedAt: seen,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	limits, ok := hb[0].Input["limits"].([]any)
	if !ok || len(limits) != 1 {
		t.Fatalf("limits = %#v", hb[0].Input["limits"])
	}
	l := limits[0].(map[string]any)
	five, _ := l["fiveHour"].(map[string]any)
	seven, _ := l["sevenDay"].(map[string]any)
	for k, want := range map[string]any{
		"account": "max-20x", "status": "allowed_warning", "rateLimitType": "five_hour",
		"resetsAt": "2026-09-25T18:00:00Z", "overageStatus": "rejected", "isUsingOverage": false,
		"observedAt": "2026-09-25T14:12:05Z",
	} {
		if l[k] != want {
			t.Fatalf("limits.%s = %#v, want %#v", k, l[k], want)
		}
	}
	if five["utilization"] != 0.83 || five["resetsAt"] != "2026-09-25T18:00:00Z" {
		t.Fatalf("fiveHour = %#v", five)
	}
	if seven["utilization"] != 0.41 || seven["resetsAt"] != "2026-09-29T09:00:00Z" {
		t.Fatalf("sevenDay = %#v", seven)
	}
}

// A heartbeat without limits has no limits key, so a platform that
// does not know the field still takes it.
func TestHeartbeatWithoutLimitsOmitsThem(t *testing.T) {
	f, gw := registeredFake(t)
	ctx := context.Background()
	for _, x := range [][]gateway.HeartbeatExtras{
		nil,
		{{}},
		{{Draining: true, Limits: []gateway.Limits{}}},
	} {
		if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 0, x...); err != nil {
			t.Fatal(err)
		}
	}
	for i, hb := range f.CallsNamed("heartbeatNode") {
		if v, ok := hb.Input["limits"]; ok {
			t.Fatalf("heartbeat %d sent limits = %#v", i, v)
		}
	}
}
