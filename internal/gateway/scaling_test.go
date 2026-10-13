package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
	_, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 2, gateway.HeartbeatExtras{
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
	// accounts rides as an empty list when the node names none: the
	// platform's nested input is non-null (2026-09-28)
	if accts, ok := s["accounts"].([]any); len(s) != 5 || !ok || len(accts) != 0 {
		t.Fatalf("scaling = %#v, want the four fields and accounts: []", s)
	}
}

// A heartbeat without scaling has no scaling key, so a platform that
// does not know the field still takes a fixed node's heartbeat.
func TestHeartbeatWithoutScalingOmitsIt(t *testing.T) {
	f, gw := registeredFake(t)
	ctx := context.Background()
	for _, x := range [][]gateway.HeartbeatExtras{nil, {{}}, {{Draining: true}}} {
		if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 0, x...); err != nil {
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
		if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 4, gateway.HeartbeatExtras{Scaling: accountsScaling()}); err != nil {
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
	if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 4, gateway.HeartbeatExtras{Scaling: accountsScaling()}); err != nil {
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
				if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, gateway.HeartbeatExtras{Scaling: accountsScaling()}); err != nil {
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
			if _, err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, gateway.HeartbeatExtras{Scaling: accountsScaling()}); err != nil {
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
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 1, gateway.HeartbeatExtras{Scaling: accountsScaling()}); err != nil {
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

// hostScaling is a ranged node whose capacity backed off from the
// host's own load and package temperature (adr-004's amendment
// 2026-10-03).
func hostScaling() *gateway.Scaling {
	sc := accountsScaling()
	sc.Reason = "host hot: load 55/18 cores"
	sc.Host = &gateway.HostScaling{Load: 55, Cores: 18, TemperatureC: 101, Hot: true}
	return sc
}

// A gateway whose HeartbeatNodeInput's scaling lists `host` gets the
// host figures that backed the capacity off: load, cores, temperature
// and whether it is judged hot (adr-004's amendment 2026-10-03).
func TestHeartbeatCarriesHostScalingTheSchemaTakes(t *testing.T) {
	f, gw := registeredFake(t)
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 4, gateway.HeartbeatExtras{Scaling: hostScaling()}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	s, _ := hb[0].Input["scaling"].(map[string]any)
	host, ok := s["host"].(map[string]any)
	if !ok {
		t.Fatalf("scaling.host = %#v", s["host"])
	}
	for k, want := range map[string]any{"load": 55.0, "cores": 18.0, "temperatureC": 101.0, "hot": true} {
		if host[k] != want {
			t.Fatalf("scaling.host.%s = %#v, want %#v", k, host[k], want)
		}
	}
	if s["reason"] != "host hot: load 55/18 cores" {
		t.Fatalf("scaling.reason = %#v", s["reason"])
	}
}

// A gateway whose scaling lacks `host` (a platform before adr-004's
// amendment 2026-10-03) gets the rest of scaling — capacity, min, max,
// reason and accounts — with no `host` key, and accepts the
// heartbeat.
func TestHeartbeatOmitsHostTheSchemaLacks(t *testing.T) {
	f, gw := registeredFake(t)
	f.UnknownFields["host"] = true
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 4, gateway.HeartbeatExtras{Scaling: hostScaling()}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	s, _ := hb[0].Input["scaling"].(map[string]any)
	if _, ok := s["host"]; ok {
		t.Fatalf("sent host the schema lacks: %#v", s["host"])
	}
	accounts, ok := s["accounts"].([]any)
	if !ok || len(accounts) != 3 {
		t.Fatalf("scaling.accounts = %#v, want the three accounts still", s["accounts"])
	}
	for k, want := range map[string]any{"capacity": 5.0, "min": 1.0, "max": 8.0, "reason": "host hot: load 55/18 cores"} {
		if s[k] != want {
			t.Fatalf("scaling.%s = %#v, want %#v", k, s[k], want)
		}
	}
}

// hostOverloadScaling is a ranged node whose host carries the
// overload-threshold readings: load per core and CPU pressure.
func hostOverloadScaling() *gateway.Scaling {
	sc := hostScaling()
	sc.Host.PerCore = 55.0 / 18
	sc.Host.CPUSomeAvg60 = 71
	return sc
}

// A gateway whose host type lists `perCore` and `cpuSomeAvg60` gets
// both figures beside load, cores, temperature and hot (adr-004's
// overload-threshold amendment).
func TestHeartbeatCarriesHostPerCoreTheSchemaTakes(t *testing.T) {
	f, gw := registeredFake(t)
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 4, gateway.HeartbeatExtras{Scaling: hostOverloadScaling()}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	s, _ := hb[0].Input["scaling"].(map[string]any)
	host, _ := s["host"].(map[string]any)
	for k, want := range map[string]any{"load": 55.0, "cores": 18.0, "hot": true, "perCore": 55.0 / 18, "cpuSomeAvg60": 71.0} {
		if host[k] != want {
			t.Fatalf("scaling.host.%s = %#v, want %#v", k, host[k], want)
		}
	}
}

// A gateway whose host type lacks `perCore` (a platform before the
// overload-threshold amendment) gets the rest of host — load, cores,
// temperature, hot and cpuSomeAvg60 when it takes that — with no
// `perCore` key, and accepts the heartbeat; each of the two fields is
// dropped on its own.
func TestHeartbeatOmitsHostPerCoreTheSchemaLacks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing string
	}{
		{"no perCore", "perCore"},
		{"no cpuSomeAvg60", "cpuSomeAvg60"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, gw := registeredFake(t)
			f.UnknownFields[tc.missing] = true
			if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 4, gateway.HeartbeatExtras{Scaling: hostOverloadScaling()}); err != nil {
				t.Fatal(err)
			}
			hb := f.CallsNamed("heartbeatNode")
			s, _ := hb[0].Input["scaling"].(map[string]any)
			host, ok := s["host"].(map[string]any)
			if !ok {
				t.Fatalf("scaling.host = %#v, want the rest of host still", s["host"])
			}
			if _, ok := host[tc.missing]; ok {
				t.Fatalf("sent %s the schema lacks: %#v", tc.missing, host[tc.missing])
			}
			other := map[string]string{"perCore": "cpuSomeAvg60", "cpuSomeAvg60": "perCore"}[tc.missing]
			if host[other] == nil {
				t.Fatalf("dropped %s too: host = %#v", other, host)
			}
			if host["load"] != 55.0 || host["cores"] != 18.0 || host["hot"] != true {
				t.Fatalf("host = %#v, want load/cores/hot still", host)
			}
		})
	}
}

// A Scaling with a nil Host sends no `host` key even when the schema
// takes it: the request is unchanged from before this field existed
// (adr-004's amendment 2026-10-03); TestHeartbeatOfAFixedNodeIsUnchanged
// below still passes unmodified, since it sends no Scaling at all.
func TestHeartbeatScalingOmitsNilHost(t *testing.T) {
	f, gw := registeredFake(t)
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 4, gateway.HeartbeatExtras{Scaling: accountsScaling()}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	s, _ := hb[0].Input["scaling"].(map[string]any)
	if _, ok := s["host"]; ok {
		t.Fatalf("sent host for a nil Host: %#v", s["host"])
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

// A fixed node on one account sends no scaling, so its heartbeat asks
// only for status and the node answer, byte for byte, and the schema
// is never probed for scaling.
func TestHeartbeatOfAFixedNodeIsUnchanged(t *testing.T) {
	f, gw := registeredFake(t)
	rec := &bodies{next: http.DefaultTransport}
	gw.HTTP = &http.Client{Transport: rec}
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v1", 2, gateway.HeartbeatExtras{}); err != nil {
		t.Fatal(err)
	}
	want := `{"query":"mutation($in: HeartbeatNodeInput!) { heartbeatNode(input: $in) { status nodeId owner { kind id name } scopes { orgId login slug accountId shares } } }","variables":{"in":{"aggregateId":"` + limitsNode + `","namespace":"runsheet","running":2,"version":"v1"}}}`
	if len(rec.sent) != 1 || rec.sent[0] != want {
		t.Fatalf("sent %q\nwant %q", rec.sent, want)
	}
	if n := len(f.ReadsNamed("__type")); n != 0 {
		t.Fatalf("a fixed node probed the schema %d times", n)
	}
}

// A heartbeat's scaling always carries `accounts` as a list, [] when the
// node has none to name: the platform's nested input is non-null
// (2026-09-28), and an omitted or null list had the whole heartbeat
// refused, so no limits ever landed.
func TestScalingAccountsAreAlwaysAList(t *testing.T) {
	f, gw := registeredFake(t)
	sc := &gateway.Scaling{Capacity: 1, Min: 1, Max: 1, Reason: "fixed"}
	if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v3", 0, gateway.HeartbeatExtras{Scaling: sc}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	s, _ := hb[len(hb)-1].Input["scaling"].(map[string]any)
	accounts, ok := s["accounts"].([]any)
	if !ok || accounts == nil || len(accounts) != 0 {
		t.Fatalf("scaling.accounts = %#v, want an empty list", s["accounts"])
	}
	// and a bare scaling (the platform without `accounts`) is a list too
	b, _ := json.Marshal(gateway.Scaling{Capacity: 1, Min: 1, Max: 1})
	if !strings.Contains(string(b), `"accounts":null`) && !strings.Contains(string(b), `"accounts":[]`) {
		t.Fatalf("marshalled scaling: %s", b)
	}
}

// A heartbeat carries commit beside version when the node knows it and
// the gateway's HeartbeatNodeInput takes it (mesh-specs ADR 013, trd §1
// Node); an empty one, or a schema without the field, sends no key and
// the heartbeat lands the first time. The schema's answer is kept for
// ScalingProbeEvery, then asked again.
func TestHeartbeatCarriesCommitTheSchemaTakes(t *testing.T) {
	f, gw := registeredFake(t)
	now := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	gw.Now = func() time.Time { return now }
	ctx := context.Background()
	for _, c := range []string{"0895d40", "0895d40", ""} {
		if _, err := gw.HeartbeatNode(ctx, limitsNode, "v0.2.188", 1, gateway.HeartbeatExtras{Commit: c}); err != nil {
			t.Fatal(err)
		}
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 3 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	for i, want := range []any{"0895d40", "0895d40", nil} {
		if got := hb[i].Input["commit"]; got != want {
			t.Fatalf("heartbeat %d: commit = %#v, want %#v", i, got, want)
		}
		if hb[i].Input["version"] != "v0.2.188" {
			t.Fatalf("heartbeat %d: version = %#v", i, hb[i].Input["version"])
		}
	}
	if n := len(f.ReadsNamed("__type")); n != 1 {
		t.Fatalf("schema probed %d times, want once", n)
	}
}

func TestHeartbeatOmitsCommitTheSchemaLacks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*gatewaytest.Fake)
	}{
		{"no commit", func(f *gatewaytest.Fake) { f.UnknownFields["commit"] = true }},
		{"no introspection", func(f *gatewaytest.Fake) { f.Unsupported["__type"] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, gw := registeredFake(t)
			tc.setup(f)
			now := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
			gw.Now = func() time.Time { return now }
			ctx := context.Background()
			for range 2 {
				if _, err := gw.HeartbeatNode(ctx, limitsNode, "v0.2.188", 1, gateway.HeartbeatExtras{Commit: "0895d40"}); err != nil {
					t.Fatal(err)
				}
			}
			hb := f.CallsNamed("heartbeatNode")
			if len(hb) != 2 {
				t.Fatalf("heartbeats: %+v (refused and resent?)", hb)
			}
			for _, c := range hb {
				if v, ok := c.Input["commit"]; ok {
					t.Fatalf("sent commit the schema lacks: %#v", v)
				}
			}
			if n := len(f.ReadsNamed("__type")); n != 1 {
				t.Fatalf("schema probed %d times, want once", n)
			}

			// the platform deploys the field; the next probe sees it
			delete(f.UnknownFields, "commit")
			delete(f.Unsupported, "__type")
			now = now.Add(gateway.ScalingProbeEvery)
			if _, err := gw.HeartbeatNode(ctx, limitsNode, "v0.2.188", 1, gateway.HeartbeatExtras{Commit: "0895d40"}); err != nil {
				t.Fatal(err)
			}
			if hb := f.CallsNamed("heartbeatNode"); hb[len(hb)-1].Input["commit"] != "0895d40" {
				t.Fatalf("commit not sent once the schema has it: %#v", hb[len(hb)-1].Input)
			}
		})
	}
}
