package node

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// The 2026-10-03 canary: the person's orgs are foghornid, then
// grapevine — a node configured to serve grapevine alone must never
// send foghornid's id as the owner registration's orgId.
const (
	foghornOrgID   = "b1111111-1111-4111-8111-111111111111"
	grapevineOrgID = "b2222222-1111-4111-8111-111111111111"
)

func foghornAndGrapevine() []gateway.Org {
	return []gateway.Org{
		{ID: foghornOrgID, Login: "foghornid"},
		{ID: grapevineOrgID, TenantSlug: "grapevine"},
	}
}

// A configured org that matches one of the person's orgs is picked —
// never the first, when it is not the match.
func TestConfiguredOrgIDPicksAConfiguredOrg(t *testing.T) {
	id, matched := ConfiguredOrgID(foghornAndGrapevine(), []string{"grapevine"})
	if !matched || id != grapevineOrgID {
		t.Fatalf("ConfiguredOrgID = %q, %v; want %q, true", id, matched, grapevineOrgID)
	}
}

// No restriction is today's behaviour: the first of the person's orgs.
func TestConfiguredOrgIDNoRestrictionIsTheFirst(t *testing.T) {
	id, matched := ConfiguredOrgID(foghornAndGrapevine(), nil)
	if !matched || id != foghornOrgID {
		t.Fatalf("ConfiguredOrgID = %q, %v; want %q, true", id, matched, foghornOrgID)
	}
}

// A configured org the person is not in matches none: no org is sent,
// never an unconfigured one.
func TestConfiguredOrgIDNoneMatchesSendsNone(t *testing.T) {
	id, matched := ConfiguredOrgID(foghornAndGrapevine(), []string{"anythink"})
	if matched || id != "" {
		t.Fatalf("ConfiguredOrgID = %q, %v; want \"\", false", id, matched)
	}
}

// configuredOrgRegistrar is a Registrar whose ConfiguredOrg and other
// steps run on gw (a real gateway.Client over a gatewaytest.Fake), the
// same way nodecmd.go's registrar() wires ConfiguredOrg — but built
// standalone here so the owner registration is driven without a
// gateway dependency in internal/commands.
func configuredOrgRegistrar(gw *gateway.Client, cfg *Config) Registrar {
	return Registrar{
		TakesOwner: gw.TakesOwner,
		OwnerField: gw.OwnerField,
		LegacyOrgs: gw.MyOrgs,
		Resolve:    func(ctx context.Context) (Scopes, error) { return ResolveScopes(ctx, gw, cfg) },
		ConfiguredOrg: func(ctx context.Context) (string, error) {
			orgs, err := gw.MyOrgs(ctx)
			if err != nil {
				return "", err
			}
			id, _ := ConfiguredOrgID(orgs, cfg.Orgs)
			return id, nil
		},
		RegisterOwner: func(ctx context.Context, nodeID, orgID, ownerID string) (*gateway.NodeAnswer, error) {
			return gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: nodeID, OrgID: orgID, OwnerID: ownerID,
				Name: cfg.Name, Slug: cfg.Slug, Harnesses: []string{"claude-code"}, Capacity: 1, Version: "test", Owner: cfg.OwnerKind()})
		},
	}
}

// Against a platform with two orgs for the user, the owner
// registration's orgId is the one node.yaml configures the node to
// serve — grapevine — never foghornid, the person's first
// (goal an-update-that-serves-no-org-is-not-confirmed, item 4). A
// second registration, configured to an org the person is not in,
// sends no orgId and notes it rather than falling back to the first.
func TestOwnedRegisterSendsTheConfiguredOrg(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Orgs = []map[string]any{
		{"id": foghornOrgID, "login": "foghornid"},
		{"id": grapevineOrgID, "tenantSlug": "grapevine"},
	}
	f.Answer = &gateway.NodeAnswer{
		NodeID: "c3333333-1111-4111-8111-111111111111",
		Owner:  gateway.NodeOwner{Kind: "person", ID: f.UserID, Name: "Chris"},
		Scopes: []gateway.ScopeAnswer{{OrgID: grapevineOrgID, Slug: "grapevine"}},
	}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{Slug: "grapevine-node", Name: "grapevine", OwnerID: f.UserID, Orgs: []string{"grapevine"}}

	if _, err := Register(context.Background(), configuredOrgRegistrar(gw, cfg), cfg, cfg.Orgs); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("registerNode")
	if len(calls) != 1 {
		t.Fatalf("registerNode calls = %d, want 1", len(calls))
	}
	if got := calls[0].Input["orgId"]; got != grapevineOrgID {
		t.Fatalf("registerNode orgId = %v, want grapevine's id %s (not foghornid's)", got, grapevineOrgID)
	}

	// Configured for an org the person is not in: no orgId sent, and
	// Register's Notes name why.
	f2 := gatewaytest.New("rs_test")
	defer f2.Close()
	f2.Orgs = []map[string]any{
		{"id": foghornOrgID, "login": "foghornid"},
		{"id": grapevineOrgID, "tenantSlug": "grapevine"},
	}
	gw2 := gateway.New(f2.URL(), "rs_test")
	cfg2 := &Config{Slug: "grapevine-node", Name: "grapevine", OwnerID: f2.UserID, Orgs: []string{"anythink"}}

	reg, err := Register(context.Background(), configuredOrgRegistrar(gw2, cfg2), cfg2, cfg2.Orgs)
	if err != nil {
		t.Fatal(err)
	}
	calls2 := f2.CallsNamed("registerNode")
	if len(calls2) != 1 || calls2[0].Input["orgId"] != "" {
		t.Fatalf("registerNode orgId = %v, want \"\" (anythink matches none)", calls2[0].Input["orgId"])
	}
	found := false
	for _, n := range reg.Notes {
		if strings.Contains(n, "none of the orgs node.yaml names is one of the person's; sending no orgId") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Notes = %v, want the no-match line", reg.Notes)
	}
}
