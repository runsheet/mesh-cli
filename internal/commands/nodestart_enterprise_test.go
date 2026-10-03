package commands

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/node"
)

// The Mini's story (goal the-mini-serves-its-enterprise-and-nothing-else):
// anythink is an enterprise miniAdminID administers, runsheet and
// wardroomid are its orgs (the platform's answer to the owner
// registration), and grapevine is an org of miniAdminID's outside it —
// the Mini's legacy per-org row lives there before this goal.
const (
	miniAdminID     = "a1111111-1111-4111-8111-111111111111"
	miniNonAdminID  = "a2222222-1111-4111-8111-111111111111"
	miniAnythinkID  = "a3333333-1111-4111-8111-111111111111"
	miniRunsheetID  = "a4444444-1111-4111-8111-111111111111"
	miniWardroomID  = "a5555555-1111-4111-8111-111111111111"
	miniGrapevineID = "a6666666-1111-4111-8111-111111111111"
)

// miniOwnedID is the node's one id on the owner contract: from (owner,
// slug), so OwnedNodeID computes the same id the first registration
// sends as its aggregateId — the fake's registerNode answer names it
// too, so the row a second start registers under is the row the first
// one made (ADR-015, OwnedNodeID).
var miniOwnedID = gateway.NodeID(uuid.MustParse(miniAdminID), "runsheet-mini").String()

// miniFake is the platform for the Mini's story.
func miniFake(t *testing.T) *gatewaytest.Fake {
	t.Helper()
	f := gatewaytest.New("rs_test")
	t.Cleanup(f.Close)
	f.UserID = miniAdminID
	f.Enterprises[miniAnythinkID] = gatewaytest.Enterprise{Slug: "anythink", DisplayName: "Anythink", Admins: []string{miniAdminID}}
	// runsheet first: AnyOrg's pick (the owner contract's orgId, which
	// the platform ignores) must not land on grapevine, or the live
	// row would carry its orgId despite serving none of it.
	f.Orgs = []map[string]any{
		{"id": miniRunsheetID, "login": "runsheet"},
		{"id": miniWardroomID, "login": "wardroomid"},
		{"id": miniGrapevineID, "tenantSlug": "grapevine"},
	}
	f.Answer = &gateway.NodeAnswer{
		NodeID: miniOwnedID,
		Owner:  gateway.NodeOwner{Kind: "enterprise", ID: miniAnythinkID, Name: "anythink"},
		Scopes: []gateway.ScopeAnswer{
			{OrgID: miniRunsheetID, Login: "runsheet"},
			{OrgID: miniWardroomID, Login: "wardroomid"},
		},
	}
	return f
}

// miniCfg is the Mini's node.yaml before this registration: owned by
// anythink but still on the per-org contract.
func miniCfg() *node.Config {
	return &node.Config{Slug: "runsheet-mini", Name: "mini", Owner: "enterprise", Enterprise: "anythink",
		Contract: node.ContractPerOrg, OwnerID: miniAdminID}
}

// seedLegacyRows pre-seeds the Mini's legacy per-org Node rows, as a
// node registered one per org before it had an owner would have left
// them (ADR-015).
func seedLegacyRows(t *testing.T, f *gatewaytest.Fake, cfg *node.Config, orgs ...string) {
	t.Helper()
	for _, org := range orgs {
		id, err := cfg.NodeIDIn(org)
		if err != nil {
			t.Fatal(err)
		}
		f.Nodes[id] = map[string]any{"id": id, "status": "online", "orgId": org, "slug": cfg.Slug, "ownerId": cfg.OwnerID}
	}
}

// liveNodes is every row of f.Nodes whose status is not retired.
func liveNodes(f *gatewaytest.Fake) map[string]map[string]any {
	live := map[string]map[string]any{}
	for id, row := range f.Nodes {
		if row["status"] != "retired" {
			live[id] = row
		}
	}
	return live
}

// register runs node.Register through the real registrar wired to a
// real gateway.Client against the fake — the same registrar `mesh node
// start` builds (internal/commands/nodecmd.go's registrar).
func register(ctx context.Context, f *gatewaytest.Fake, cfg *node.Config) (node.Registered, error) {
	gw := gateway.New(f.URL(), "rs_test")
	return node.Register(ctx, registrar(gw, cfg), cfg, cfg.Orgs)
}

// Scenario 1 (the Mini's story): a node configured `owner: enterprise,
// enterprise: anythink, contract: per-org` on an enterprise admin's
// token registers exactly one row with ownerKind enterprise and ownerId
// anythink's id, retires its legacy per-org rows, and serves only the
// answer's scopes.
func TestEnterpriseRegistrationRetiresTheLegacyRows(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := miniFake(t)
	cfg := miniCfg()
	seedLegacyRows(t, f, cfg, miniGrapevineID, miniRunsheetID)

	reg, err := register(context.Background(), f, cfg)
	if err != nil {
		t.Fatal(err)
	}

	live := liveNodes(f)
	if len(live) != 1 {
		t.Fatalf("live nodes = %+v, want exactly one", live)
	}
	for _, row := range live {
		if row["ownerKind"] != "enterprise" || row["ownerId"] != miniAnythinkID {
			t.Fatalf("live row = %+v, want ownerKind enterprise, ownerId %s", row, miniAnythinkID)
		}
		if row["orgId"] == miniGrapevineID {
			t.Fatalf("live row carries grapevine's orgId: %+v", row)
		}
	}
	for _, org := range []string{miniGrapevineID, miniRunsheetID} {
		id, _ := cfg.NodeIDIn(org)
		if f.Nodes[id]["status"] != "retired" {
			t.Fatalf("legacy row of %s = %+v, want retired", org, f.Nodes[id])
		}
	}
	if names := reg.Scopes.Names(); !slices.Equal(names, []string{"runsheet", "wardroomid"}) {
		t.Fatalf("scopes = %q, want the answer's", names)
	}

	if err := node.SaveRegistration(cfg, reg, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := node.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Contract != node.ContractOwner || saved.EnterpriseID != miniAnythinkID || saved.NodeID != miniOwnedID {
		t.Fatalf("saved node.yaml contract %q enterprise_id %q node_id %q", saved.Contract, saved.EnterpriseID, saved.NodeID)
	}

	lines := node.RegistrationLines(cfg, reg)
	if lines[0] != "node mini (runsheet-mini) owned by anythink (enterprise), serving runsheet, wardroomid" {
		t.Fatalf("start line = %q", lines[0])
	}
	var retiredLines int
	for _, l := range lines {
		if strings.HasPrefix(l, "retired legacy node ") {
			retiredLines++
		}
	}
	if retiredLines != 2 {
		t.Fatalf("lines = %q, want two `retired legacy node` lines", lines)
	}
}

// Scenario 2: `orgs: [runsheet]` restricts the enterprise's own scopes
// further — the node serves runsheet alone, and the restriction shows
// in the same registration line a person or `mesh node register` reads
// (RegistrationLines' StartLine).
func TestEnterpriseRegistrationOrgsRestrictsFurther(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := miniFake(t)
	cfg := miniCfg()
	cfg.Orgs = []string{"runsheet"}

	reg, err := register(context.Background(), f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if names := reg.Scopes.Names(); !slices.Equal(names, []string{"runsheet"}) {
		t.Fatalf("scopes = %q, want runsheet alone", names)
	}
	lines := node.RegistrationLines(cfg, reg)
	if lines[0] != "node mini (runsheet-mini) owned by anythink (enterprise), serving runsheet" {
		t.Fatalf("start line = %q, want the orgs: restriction reflected in it", lines[0])
	}
}

// Scenario 3: a token whose person does not administer anythink is
// refused by the fake's adminOnEnterprise, the same way the platform
// refuses it, and nothing is retired. `enterprise:` is given as
// anythink's id so the refusal comes from the registerNode mutation
// itself, not from the node's own myEnterprises lookup finding no
// enterprise to resolve the slug against (a non-admin's myEnterprises
// lists none).
func TestEnterpriseRegistrationRefusedForANonAdmin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := miniFake(t)
	f.UserID = miniNonAdminID
	cfg := miniCfg()
	cfg.OwnerID = miniNonAdminID
	cfg.Enterprise = miniAnythinkID
	seedLegacyRows(t, f, cfg, miniGrapevineID, miniRunsheetID)

	_, err := register(context.Background(), f, cfg)
	if err == nil || !strings.Contains(err.Error(), "adminOnEnterprise") {
		t.Fatalf("err = %v, want an adminOnEnterprise refusal", err)
	}
	for _, org := range []string{miniGrapevineID, miniRunsheetID} {
		id, _ := cfg.NodeIDIn(org)
		if f.Nodes[id]["status"] == "retired" {
			t.Fatalf("legacy row of %s retired on a refused registration", org)
		}
	}
	// the two legacy rows stay exactly as seeded — online, never
	// retired — and no enterprise row was created
	if len(f.Nodes) != 2 {
		t.Fatalf("f.Nodes = %+v, want only the two pre-seeded legacy rows", f.Nodes)
	}
}

// Scenario 4: a second start on the saved node.yaml — contract already
// owner — registers the same one id and retires nothing: the contract's
// transition is the trigger, and a restart is not a re-sweep.
func TestEnterpriseRegistrationSecondStartRetiresNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := miniFake(t)
	cfg := miniCfg()
	seedLegacyRows(t, f, cfg, miniGrapevineID, miniRunsheetID)
	reg, err := register(context.Background(), f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.SaveRegistration(cfg, reg, nil); err != nil {
		t.Fatal(err)
	}

	saved, err := node.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	second, err := register(context.Background(), f, saved)
	if err != nil {
		t.Fatal(err)
	}
	if second.Owned == nil || second.Owned.NodeID != miniOwnedID {
		t.Fatalf("second registration id = %+v, want the same %q", second.Owned, miniOwnedID)
	}
	for _, l := range second.Notes {
		if strings.Contains(l, "legacy node") {
			t.Fatalf("second start retired a legacy row: %q", l)
		}
	}
	if live := liveNodes(f); len(live) != 1 {
		t.Fatalf("live nodes after the second start = %+v, want exactly one", live)
	}
}

// Scenario 1 again on a platform whose RegisterNodeInput still spells
// the owner kind `owner` (OwnerFieldLegacy): the node sends `owner`,
// never `ownerKind`, and the result is the same.
func TestEnterpriseRegistrationWithLegacyOwnerField(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := miniFake(t)
	f.OwnerFieldLegacy = true
	cfg := miniCfg()
	seedLegacyRows(t, f, cfg, miniGrapevineID, miniRunsheetID)

	reg, err := register(context.Background(), f, cfg)
	if err != nil {
		t.Fatal(err)
	}

	calls := f.CallsNamed("registerNode")
	if len(calls) != 1 {
		t.Fatalf("registerNode calls = %+v, want one", calls)
	}
	in := calls[0].Input
	if in["owner"] != "enterprise" {
		t.Fatalf("registerNode owner = %v, want enterprise sent as `owner`", in["owner"])
	}
	if _, ok := in["ownerKind"]; ok {
		t.Fatalf("registerNode sent ownerKind beside owner: %+v", in)
	}

	live := liveNodes(f)
	if len(live) != 1 {
		t.Fatalf("live nodes = %+v, want exactly one", live)
	}
	for _, row := range live {
		if row["ownerKind"] != "enterprise" || row["ownerId"] != miniAnythinkID {
			t.Fatalf("live row = %+v, want ownerKind enterprise, ownerId %s", row, miniAnythinkID)
		}
	}
	for _, org := range []string{miniGrapevineID, miniRunsheetID} {
		id, _ := cfg.NodeIDIn(org)
		if f.Nodes[id]["status"] != "retired" {
			t.Fatalf("legacy row of %s = %+v, want retired", org, f.Nodes[id])
		}
	}
	if names := reg.Scopes.Names(); !slices.Equal(names, []string{"runsheet", "wardroomid"}) {
		t.Fatalf("scopes = %q, want the answer's", names)
	}
}
