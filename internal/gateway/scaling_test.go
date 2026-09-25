package gateway_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
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

// accountsScaling is a ranged node on two subscriptions and an api key.
func accountsScaling() *gateway.Scaling {
	return &gateway.Scaling{Capacity: 5, Min: 1, Max: 8, Reason: "accounts", Accounts: []gateway.AccountScaling{
		{Name: "max-a", Kind: "subscription", Slots: 1, Reason: "five-hour pace"},
		{Name: "max-b", Kind: "subscription", Slots: 1, Reason: "five-hour pace"},
		{Name: "key", Kind: "api", Slots: 2, UrgentOnlySlots: 1, Reason: "daily cap"},
	}}
}

// A gateway whose HeartbeatNodeInput has scaling with accounts gets one
// entry per account — name, kind, slots, urgent-only slots and reason,
// nothing else — and is asked once, not every beat; after
// ScalingProbeEvery it is asked again.
func TestHeartbeatCarriesAccountSlotsTheSchemaTakes(t *testing.T) {
	f, gw := registeredFake(t)
	now := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	gw.Now = func() time.Time { return now }
	ctx := context.Background()
	for range 2 {
		if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 4, gateway.HeartbeatExtras{Scaling: accountsScaling()}); err != nil {
			t.Fatal(err)
		}
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 2 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	s, _ := hb[1].Input["scaling"].(map[string]any)
	for k, want := range map[string]any{"capacity": 5.0, "min": 1.0, "max": 8.0, "reason": "accounts"} {
		if s[k] != want {
			t.Fatalf("scaling.%s = %#v, want %#v", k, s[k], want)
		}
	}
	accounts, _ := s["accounts"].([]any)
	want := []map[string]any{
		{"name": "max-a", "kind": "subscription", "slots": 1.0, "urgentOnlySlots": 0.0, "reason": "five-hour pace"},
		{"name": "max-b", "kind": "subscription", "slots": 1.0, "urgentOnlySlots": 0.0, "reason": "five-hour pace"},
		{"name": "key", "kind": "api", "slots": 2.0, "urgentOnlySlots": 1.0, "reason": "daily cap"},
	}
	if len(accounts) != len(want) {
		t.Fatalf("scaling.accounts = %#v", s["accounts"])
	}
	for i, w := range want {
		a, _ := accounts[i].(map[string]any)
		if len(a) != len(w) {
			t.Fatalf("accounts[%d] = %#v, want exactly %#v", i, a, w)
		}
		for k, v := range w {
			if a[k] != v {
				t.Fatalf("accounts[%d].%s = %#v, want %#v", i, k, a[k], v)
			}
		}
	}
	if n := len(f.ReadsNamed("__type")); n != 1 {
		t.Fatalf("schema probed %d times, want once", n)
	}
	now = now.Add(gateway.ScalingProbeEvery)
	if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 4, gateway.HeartbeatExtras{Scaling: accountsScaling()}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.ReadsNamed("__type")); n != 2 {
		t.Fatalf("schema probed %d times after ScalingProbeEvery, want 2", n)
	}
}

// A gateway whose schema lacks scaling — or will not answer the probe —
// gets a heartbeat without it, taken first time, with no fallback; the
// cached no holds until the probe is due again, when a deploy that
// added the field is seen.
func TestHeartbeatOmitsScalingTheSchemaLacks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*gatewaytest.Fake)
	}{
		{"no scaling", func(f *gatewaytest.Fake) { f.UnknownFields["scaling"] = true }},
		{"no introspection", func(f *gatewaytest.Fake) { f.Unsupported["__type"] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, gw := registeredFake(t)
			tc.setup(f)
			now := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
			gw.Now = func() time.Time { return now }
			ctx := context.Background()
			for range 2 {
				if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, gateway.HeartbeatExtras{Scaling: accountsScaling()}); err != nil {
					t.Fatal(err)
				}
			}
			hb := f.CallsNamed("heartbeatNode")
			if len(hb) != 2 {
				t.Fatalf("heartbeats: %+v", hb)
			}
			for _, c := range hb {
				if v, ok := c.Input["scaling"]; ok {
					t.Fatalf("sent scaling the schema lacks: %#v", v)
				}
			}
			if n := len(f.ReadsNamed("__type")); n != 1 {
				t.Fatalf("schema probed %d times, want once", n)
			}

			// the platform deploys the field; the next probe sees it
			delete(f.UnknownFields, "scaling")
			delete(f.Unsupported, "__type")
			now = now.Add(gateway.ScalingProbeEvery)
			if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, gateway.HeartbeatExtras{Scaling: accountsScaling()}); err != nil {
				t.Fatal(err)
			}
			if hb := f.CallsNamed("heartbeatNode"); hb[len(hb)-1].Input["scaling"] == nil {
				t.Fatalf("scaling not sent once the schema has it: %#v", hb[len(hb)-1].Input)
			}
		})
	}
}

// A gateway whose scaling predates the per-account slots (ADR-004's
// four fields) gets those four, and the heartbeat lands.
func TestHeartbeatScalingWithoutAccountsTheSchemaLacks(t *testing.T) {
	f, gw := registeredFake(t)
	f.UnknownFields["accounts"] = true
	if err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 1, gateway.HeartbeatExtras{Scaling: accountsScaling()}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	s, _ := hb[0].Input["scaling"].(map[string]any)
	if len(s) != 4 || s["capacity"] != 5.0 || s["reason"] != "accounts" {
		t.Fatalf("scaling = %#v, want the four fields only", s)
	}
}

// bodies records every request body the client sends.
type bodies struct {
	mu   sync.Mutex
	sent []string
	next http.RoundTripper
}

func (b *bodies) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	b.sent = append(b.sent, string(raw))
	b.mu.Unlock()
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return b.next.RoundTrip(r)
}

// A fixed node on one account sends no scaling, so its heartbeat is the
// request it always was, byte for byte, and the schema is never probed.
func TestHeartbeatOfAFixedNodeIsUnchanged(t *testing.T) {
	f, gw := registeredFake(t)
	rec := &bodies{next: http.DefaultTransport}
	gw.HTTP = &http.Client{Transport: rec}
	if err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 2, gateway.HeartbeatExtras{}); err != nil {
		t.Fatal(err)
	}
	want := `{"query":"mutation($in: HeartbeatNodeInput!) { heartbeatNode(input: $in) { status } }","variables":{"in":{"aggregateId":"` + limitsNode + `","namespace":"runsheet","running":2,"version":"v1"}}}`
	if len(rec.sent) != 1 || rec.sent[0] != want {
		t.Fatalf("sent %q\nwant %q", rec.sent, want)
	}
	if n := len(f.ReadsNamed("__type")); n != 0 {
		t.Fatalf("a fixed node probed the schema %d times", n)
	}
}
