package meshapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A take's brief enforces the egress Mesh granted the lease: the
// granted hosts (the kind's baseline plus the offer's extra_hosts the
// tenant's policy allows) replace the kind's baseline in Egress, so a
// repository's declared hosts reach the relay (GrapevineNetwork/website's
// fonts.googleapis.com, 2026-11-16); MCP, model and registries stay the
// kind's.
func TestTakeEnforcesGrantedEgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"lease_id":"l1","job_id":"j1"},"brief":{"job_id":"j1","lease_id":"l1","kind":"build",
			"egress":{"hosts":["registry.npmjs.org"],"mcp":["runsheet"],"model":true,"registries":["npm"]},
			"granted":{"hosts":["registry.npmjs.org","fonts.googleapis.com","fonts.gstatic.com"],"mcp":["runsheet"],"model":true}}}`))
	}))
	defer srv.Close()
	c := &meshapi.Client{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "machine", nil }}
	tk, err := c.Take(context.Background(), meshapi.TakeRequest{NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	e := tk.Brief.Egress
	if e == nil || !slices.Equal(e.Hosts, []string{"registry.npmjs.org", "fonts.googleapis.com", "fonts.gstatic.com"}) {
		t.Fatalf("egress %+v: the granted hosts are not enforced", e)
	}
	if !slices.Equal(e.MCP, []string{"runsheet"}) || !e.Model || !slices.Equal(e.Registries, []string{"npm"}) {
		t.Fatalf("egress %+v: mcp, model or registries changed", e)
	}
}

// A grant narrows as well as widens: hosts the tenant's policy denied
// are gone from Egress, even to none.
func TestApplyGrantedNarrows(t *testing.T) {
	b := &meshapi.Brief{Egress: &meshapi.Egress{Hosts: []string{"a.example.com", "b.example.com"}}, Granted: &meshapi.Egress{Hosts: []string{}}}
	b.ApplyGranted()
	if len(b.Egress.Hosts) != 0 {
		t.Fatalf("hosts %v survived an empty grant", b.Egress.Hosts)
	}
}

// No grant (an older Mesh) keeps the kind's baseline; no egress (an
// open kind) stays open, whatever the grant says; applying twice is
// applying once.
func TestApplyGrantedLeavesWhatItShould(t *testing.T) {
	b := &meshapi.Brief{Egress: &meshapi.Egress{Hosts: []string{"a.example.com"}}}
	b.ApplyGranted()
	if !slices.Equal(b.Egress.Hosts, []string{"a.example.com"}) {
		t.Fatalf("no grant changed hosts to %v", b.Egress.Hosts)
	}
	open := &meshapi.Brief{Granted: &meshapi.Egress{Hosts: []string{"a.example.com"}}}
	open.ApplyGranted()
	if open.Egress != nil {
		t.Fatal("a grant closed an open kind's egress")
	}
	g := &meshapi.Brief{Egress: &meshapi.Egress{Hosts: []string{"a.example.com"}}, Granted: &meshapi.Egress{Hosts: []string{"a.example.com", "x.example.com"}}}
	g.ApplyGranted()
	g.ApplyGranted()
	if !slices.Equal(g.Egress.Hosts, []string{"a.example.com", "x.example.com"}) {
		t.Fatalf("hosts %v", g.Egress.Hosts)
	}
	var nilBrief *meshapi.Brief
	nilBrief.ApplyGranted()
}
