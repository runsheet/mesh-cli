package gateway_test

import (
	"context"
	"reflect"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/tools"
)

// The claim-time read of an item carries its tools, remote and local,
// secrets and all (trd §3 Tools).
func TestSessionCarriesTools(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.SessionCredentials[itemID] = []map[string]any{{"id": "c1", "label": "max-a", "kind": "subscription", "secret": "sk-ant-oat01-x"}}
	f.SessionTools[itemID] = []map[string]any{
		{"name": "linear", "transport": "remote", "url": "https://mcp.linear.app/mcp", "headers": map[string]any{"X-Team": "eng"},
			"secrets": map[string]any{"Authorization": "Bearer lin_x"}, "required": true, "roles": []any{"build", "plan"}, "pricePerCallCents": 2},
		{"name": "pg", "transport": "local", "runtime": "npx", "command": "@modelcontextprotocol/server-postgres", "args": []any{"--ro"},
			"env": map[string]any{"PGHOST": "db"}, "secrets": map[string]any{"PGPASSWORD": "pw"}, "required": false},
	}
	gw := gateway.New(f.URL(), "rs_test")
	got, err := gw.Session(context.Background(), gateway.SessionRef{Kind: "build", ItemID: itemID}, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	want := []tools.Tool{
		{Name: "linear", Transport: tools.Remote, URL: "https://mcp.linear.app/mcp", Headers: map[string]string{"X-Team": "eng"},
			Secrets: map[string]string{"Authorization": "Bearer lin_x"}, Required: true, Roles: []string{"build", "plan"}, PricePerCallCents: 2},
		{Name: "pg", Transport: tools.Local, Runtime: tools.Npx, Command: "@modelcontextprotocol/server-postgres", Args: []string{"--ro"},
			Env: map[string]string{"PGHOST": "db"}, Secrets: map[string]string{"PGPASSWORD": "pw"}},
	}
	if !reflect.DeepEqual(got.Tools, want) {
		t.Fatalf("tools = %+v\nwant %+v", got.Tools, want)
	}
	if len(got.Credentials) != 1 || got.Credentials[0].Label != "max-a" {
		t.Fatalf("credentials = %v", got.Credentials)
	}
}

// A read without the field — no tools configured, or a platform older
// than tools-in-the-console — decodes to nil: no console tools.
func TestSessionWithoutTools(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.SessionCredentials[itemID] = []map[string]any{{"id": "c1", "label": "max-a", "kind": "subscription", "secret": "s"}}
	gw := gateway.New(f.URL(), "rs_test")
	ref := gateway.SessionRef{Kind: "build", ItemID: itemID}
	got, err := gw.Session(context.Background(), ref, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tools != nil || len(got.Credentials) != 1 {
		t.Fatalf("session = %+v, want its credential and nil tools", got)
	}

	// an older platform refuses the selection: the client asks again
	// without it, and from then on asks for credentials alone
	old := gatewaytest.New("rs_test")
	defer old.Close()
	old.UnknownFields["tools"] = true
	old.SessionCredentials[itemID] = f.SessionCredentials[itemID]
	old.SessionTools[itemID] = []map[string]any{{"name": "x", "transport": "remote", "url": "https://x"}}
	gw = gateway.New(old.URL(), "rs_test")
	for i := 0; i < 2; i++ {
		got, err := gw.Session(context.Background(), ref, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Tools != nil || len(got.Credentials) != 1 {
			t.Fatalf("read %d: session = %+v, want its credential and nil tools", i, got)
		}
	}
	creds, err := gw.SessionCredentials(context.Background(), ref, nodeID)
	if err != nil || len(creds) != 1 {
		t.Fatalf("SessionCredentials = %v, %v", creds, err)
	}
}

// The heartbeat carries runtimes and adapters when set, and neither key
// when not, so a platform without the fields still takes it.
func TestHeartbeatRuntimesAdapters(t *testing.T) {
	f, gw := registeredFake(t)
	ctx := context.Background()
	if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 1, gateway.HeartbeatExtras{Runtimes: []string{"npx", "docker"}, Adapters: []string{"claude-code"}}); err != nil {
		t.Fatal(err)
	}
	hb := f.CallsNamed("heartbeatNode")
	if len(hb) != 1 {
		t.Fatalf("heartbeats: %+v", hb)
	}
	if got := hb[0].Input["runtimes"]; !reflect.DeepEqual(got, []any{"npx", "docker"}) {
		t.Fatalf("runtimes = %#v", got)
	}
	if got := hb[0].Input["adapters"]; !reflect.DeepEqual(got, []any{"claude-code"}) {
		t.Fatalf("adapters = %#v", got)
	}
	for _, x := range [][]gateway.HeartbeatExtras{nil, {{}}, {{Draining: true, Runtimes: []string{}}}} {
		if err := gw.HeartbeatNode(ctx, limitsNode, "v1", 0, x...); err != nil {
			t.Fatal(err)
		}
	}
	for i, hb := range f.CallsNamed("heartbeatNode")[1:] {
		for _, k := range []string{"runtimes", "adapters"} {
			if v, ok := hb.Input[k]; ok {
				t.Fatalf("heartbeat %d sent %s = %#v", i+1, k, v)
			}
		}
	}
}
