package node

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const ownerOrgGrapevine = "9a9a9a9a-3333-4333-8333-9a9a9a9a9a9a"

// ownerOrgRegistrar is Register's steps on a real client against the
// fake, as nodecmd's registrar wires them for owner: org.
func ownerOrgRegistrar(gw *gateway.Client, cfg *Config, resolves *int) Registrar {
	return Registrar{
		TakesOwner: gw.TakesOwner,
		ResolveOrg: func(ctx context.Context, slug string) (string, error) {
			*resolves++
			orgs, err := gw.MyOrgs(ctx)
			if err != nil {
				return "", err
			}
			return FindOrg(slug, orgs)
		},
		ConfiguredOrg: func(context.Context) (string, error) {
			panic("owner: org sends its own org as orgId, never a configured one")
		},
		RegisterOwner: func(ctx context.Context, nodeID, orgID, ownerID string) (*gateway.NodeAnswer, error) {
			return gw.RegisterNode(ctx, gateway.RegisterNodeInput{NodeID: nodeID, OrgID: orgID, OwnerID: ownerID, Name: cfg.Name, Slug: cfg.Slug, Owner: cfg.OwnerKind()})
		},
	}
}

// Register on owner: org resolves the org's id from the token's orgs
// once, derives the node id from it and caches org_id; the next
// registration reads the cache and never resolves again.
func TestRegisterOwnerOrgResolvesTheOrgOnce(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Orgs = []map[string]any{{"id": ownerOrgGrapevine, "login": "grapevine", "name": "Grapevine"}}
	f.Answer = nil
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{Slug: "mini", Name: "mini", Owner: "org", OrgLogin: "grapevine"}
	resolves := 0
	ctx := context.Background()
	reg, err := Register(ctx, ownerOrgRegistrar(gw, cfg, &resolves), cfg, cfg.Orgs)
	if err != nil {
		t.Fatal(err)
	}
	want := gateway.NodeOwnerID("org", uuid.MustParse(ownerOrgGrapevine), "mini").String()
	if reg.Owned == nil || reg.Owned.NodeID != want || reg.OrgID != ownerOrgGrapevine || reg.EnterpriseID != "" {
		t.Fatalf("Registered = %+v, want node %s org_id %s", reg, want, ownerOrgGrapevine)
	}
	if err := SaveRegistration(cfg, reg, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if cfg.OrgID != ownerOrgGrapevine || cfg.NodeID != want || cfg.NodeIDSource != IDDerived {
		t.Fatalf("cfg org_id %q node_id %q source %q", cfg.OrgID, cfg.NodeID, cfg.NodeIDSource)
	}
	if _, err := Register(ctx, ownerOrgRegistrar(gw, cfg, &resolves), cfg, cfg.Orgs); err != nil {
		t.Fatal(err)
	}
	if resolves != 1 {
		t.Fatalf("ResolveOrg asked %d times, want 1 (the second registration reads org_id)", resolves)
	}
	if f.SignInCalls != 0 {
		t.Fatalf("signIn called %d times, want 0", f.SignInCalls)
	}
}

// FindOrg matches an org by slug, tenant slug, login or id, and names
// the token's orgs when none matches.
func TestFindOrgOwnerOrg(t *testing.T) {
	orgs := []gateway.Org{{ID: "a", Login: "acme"}, {ID: "g", Slug: "grapevine", TenantSlug: "grapevine-co"}}
	for in, want := range map[string]string{"acme": "a", "GRAPEVINE": "g", "grapevine-co": "g", ownerOrgGrapevine: ownerOrgGrapevine} {
		if got, err := FindOrg(in, orgs); err != nil || got != want {
			t.Errorf("FindOrg(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := FindOrg("wardroomid", orgs); err == nil || !strings.Contains(err.Error(), "acme, grapevine-co") {
		t.Errorf("FindOrg(wardroomid) err = %v", err)
	}
}

// The start line of an org-owned node names the org as its owner, with
// its kind, and the one org it serves.
func TestStartLineOwnerOrg(t *testing.T) {
	cfg := &Config{Name: "mini", Slug: "runsheet-mini", Owner: "org", OrgLogin: "grapevine"}
	r := Registered{
		Owned:  &Owned{NodeID: "n", Owner: Owner{Kind: "org", ID: ownerOrgGrapevine, Name: "Grapevine"}},
		Scopes: Scopes{{Login: "Grapevine", ID: ownerOrgGrapevine, NodeID: "n"}},
	}
	want := "node mini (runsheet-mini) owned by Grapevine (org), serving Grapevine"
	if got := StartLine(cfg, r); got != want {
		t.Fatalf("StartLine = %q, want %q", got, want)
	}
}
