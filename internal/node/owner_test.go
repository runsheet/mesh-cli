package node

import (
	"context"
	"os"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

const (
	ownerID     = "44444444-4444-4444-8444-444444444444"
	answerNode  = "55555555-5555-4555-8555-555555555555"
	orgWardroom = "77777777-7777-4777-8777-777777777777"
	orgGrape    = "99999999-9999-4999-8999-999999999999"
	orgFoghorn  = "bbbbbbbb-3333-4333-8333-bbbbbbbbbbbb"
)

// ownerAnswer is the platform's answer for a person's node serving
// acme, wardroomid and grapevine — an org without a GitHub login.
func ownerAnswer() *gateway.NodeAnswer {
	return &gateway.NodeAnswer{
		NodeID: answerNode,
		Owner:  gateway.NodeOwner{Kind: "person", ID: ownerID, Name: "Chris"},
		Scopes: []gateway.ScopeAnswer{
			{OrgID: orgID, Login: "acme", Slug: "acme", AccountID: acctID},
			{OrgID: orgWardroom, Login: "wardroomid", Slug: "wardroomid", AccountID: "88888888-8888-4888-8888-888888888888"},
			{OrgID: orgGrape, Login: "", Slug: "grapevine", AccountID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"},
		},
	}
}

// A platform that takes the owner is registered with once, under the
// node's one id and its owner; the answer's three orgs are the scopes,
// each carrying that one id.
func TestRegisterOnceWithAnOwnerServesTheAnswersOrgs(t *testing.T) {
	f := seed(t)
	f.Answer = ownerAnswer()
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{Slug: "mini", Name: "Mini", OwnerID: ownerID, Capacity: Fixed(2)}

	scopes, owner, err := RegisterOnce(context.Background(), gw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil || owner.Kind != "person" || owner.Name != "Chris" {
		t.Fatalf("owner = %+v", owner)
	}
	if len(scopes) != 3 {
		t.Fatalf("scopes = %+v", scopes)
	}
	if ids := scopes.NodeIDs(); len(ids) != 1 || ids[0] != answerNode {
		t.Fatalf("node ids = %v, want the one %s", ids, answerNode)
	}
	for _, sc := range scopes {
		if sc.NodeID != answerNode {
			t.Fatalf("scope %s carries %s", sc.Name(), sc.NodeID)
		}
	}
	if cfg.NodeID != answerNode {
		t.Fatalf("cfg.NodeID = %q, want the answer's for the write-back", cfg.NodeID)
	}
	regs := f.CallsNamed("registerNode")
	if len(regs) != 1 {
		t.Fatalf("registerNode called %d times, want once", len(regs))
	}
	own, _ := (&Config{Slug: "mini", OwnerID: ownerID}).OwnNodeID()
	if in := regs[0].Input; in["aggregateId"] != own || in["owner"] != "person" || in["ownerId"] != ownerID || in["slug"] != "mini" || in["orgId"] != orgID {
		t.Fatalf("registerNode input = %v (own id %s)", in, own)
	}
}

// An enterprise's node declares so, and an answer with no org is an
// error naming the owner.
func TestRegisterOnceEnterpriseAndNoScopes(t *testing.T) {
	f := seed(t)
	f.Answer = &gateway.NodeAnswer{NodeID: answerNode, Owner: gateway.NodeOwner{Kind: "enterprise", Name: "anythink"}}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{Slug: "mini", Name: "Mini", OwnerID: ownerID, Owner: OwnerEnterprise, Capacity: Fixed(1)}
	if _, _, err := RegisterOnce(context.Background(), gw, cfg); err == nil || !strings.Contains(err.Error(), "enterprise") || !strings.Contains(err.Error(), "serves no org") {
		t.Fatalf("err = %v", err)
	}
	if in := f.CallsNamed("registerNode")[0].Input; in["owner"] != "enterprise" {
		t.Fatalf("owner sent = %v", in["owner"])
	}
}

// node.yaml's orgs keeps the answer's orgs to those it names — by login,
// or by slug for an org without one — and names one the owner does not
// serve as an error.
func TestRegisterOnceHonoursTheOrgsRestriction(t *testing.T) {
	f := seed(t)
	f.Answer = ownerAnswer()
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{Slug: "mini", Name: "Mini", OwnerID: ownerID, Orgs: []string{"Grapevine", "acme"}, Capacity: Fixed(1)}
	scopes, _, err := RegisterOnce(context.Background(), gw, cfg)
	if err != nil || strings.Join(scopes.Logins(), ",") != "acme,grapevine" {
		t.Fatalf("scopes = %v, %v", scopes.Logins(), err)
	}
	cfg = &Config{Slug: "mini", Name: "Mini", OwnerID: ownerID, Orgs: []string{"acme", "initech"}, Capacity: Fixed(1)}
	if _, _, err := RegisterOnce(context.Background(), gw, cfg); err == nil || !strings.Contains(err.Error(), "initech") {
		t.Fatalf("an org the owner does not serve must refuse: %v", err)
	}
}

// An org without a GitHub login is named by its slug.
func TestScopeWithoutLoginIsNamedBySlug(t *testing.T) {
	scopes := ScopesFrom(ownerAnswer(), &Config{Slug: "mini", OwnerID: ownerID})
	if got := strings.Join(scopes.Logins(), ","); got != "acme,wardroomid,grapevine" {
		t.Fatalf("Logins() = %q", got)
	}
	grape, ok := scopes.ByOrg(orgGrape)
	if !ok || grape.Name() != "grapevine" || grape.Login != "grapevine" || grape.Slug != "grapevine" {
		t.Fatalf("grapevine scope = %+v (name %q)", grape, grape.Name())
	}
	if scopes.LoginOf(orgGrape) != "grapevine" {
		t.Fatalf("LoginOf = %q", scopes.LoginOf(orgGrape))
	}
	if (Scope{Slug: "grapevine"}).Name() != "grapevine" {
		t.Fatal("a scope with only a slug is named by it")
	}

	// the same through MyOrgs, on a platform that takes no owner
	f := seed(t)
	f.Orgs = append(f.Orgs, map[string]any{"id": orgGrape, "login": "", "tenantSlug": "grapevine", "accountId": acctID})
	legacy, err := ResolveScopes(context.Background(), gateway.New(f.URL(), "rs_test"), &Config{Slug: "box", Orgs: []string{"grapevine"}})
	if err != nil || len(legacy) != 1 || legacy[0].Name() != "grapevine" {
		t.Fatalf("resolved = %+v, %v", legacy, err)
	}
}

// Given an answer naming a fourth org, the Reloader serves four — the
// new one on this tick — with no MyOrgs read and no registration.
func TestReloaderServesTheAnswersNewOrgWithoutMyOrgsOrRegister(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{Slug: "mini", Name: "Mini", OwnerID: ownerID, Capacity: Fixed(1)}
	answer := ownerAnswer()
	start := ScopesFrom(answer, cfg)
	registered := 0
	load := func() (*Config, error) { c := *cfg; return &c, nil }
	reload := Reloader(gw, cfg, start, load, func(context.Context, Scope) error { registered++; return nil }, func() *gateway.NodeAnswer { return answer })

	answer.Scopes = append(answer.Scopes, gateway.ScopeAnswer{OrgID: orgFoghorn, Login: "foghornid", Slug: "foghornid", AccountID: acctID})
	got, err := reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Scopes) != 4 || got.Scopes[3].Name() != "foghornid" {
		t.Fatalf("scopes = %v", got.Scopes.Logins())
	}
	if ids := got.Scopes.NodeIDs(); len(ids) != 1 || ids[0] != answerNode {
		t.Fatalf("node ids = %v", ids)
	}
	if n := len(f.ReadsNamed("myOrgs")); n != 0 {
		t.Fatalf("MyOrgs read %d times", n)
	}
	if registered != 0 || len(f.CallsNamed("registerNode")) != 0 {
		t.Fatalf("registered %d / %d times", registered, len(f.CallsNamed("registerNode")))
	}

	// an answer with no scopes (an older platform) runs today's path
	answer.Scopes = nil
	if _, err := reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.ReadsNamed("myOrgs")); n != 1 {
		t.Fatalf("MyOrgs read %d times on the legacy path, want 1", n)
	}
}

// A platform that takes no owner keeps today's registration: a Node per
// org of the person, each its own id, registered per org.
func TestRegisterOnceWithoutOwnerRegistersPerOrg(t *testing.T) {
	f := seed(t)
	f.UnknownFields = map[string]bool{"owner": true}
	f.Orgs = append(f.Orgs,
		map[string]any{"id": orgWardroom, "login": "wardroomid", "accountId": acctID},
		map[string]any{"id": orgGrape, "login": "", "tenantSlug": "grapevine", "accountId": acctID})
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{Slug: "box", Name: "Box", OwnerID: ownerID, Capacity: Fixed(1)}

	scopes, owner, err := RegisterOnce(context.Background(), gw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if owner.Kind != OwnerPerson || owner.Name != ownerID {
		t.Fatalf("owner = %+v", owner)
	}
	if len(scopes) != 3 || len(scopes.NodeIDs()) != 3 {
		t.Fatalf("scopes = %+v", scopes)
	}
	for _, sc := range scopes {
		want, _ := cfg.NodeIDIn(sc.ID)
		if sc.NodeID != want {
			t.Fatalf("%s: node id %s, want its per-org %s", sc.Name(), sc.NodeID, want)
		}
	}
	regs := f.CallsNamed("registerNode")
	if len(regs) != 3 {
		t.Fatalf("registerNode called %d times, want 3", len(regs))
	}
	for _, r := range regs {
		if _, ok := r.Input["owner"]; ok {
			t.Fatalf("owner sent to a platform that takes none: %v", r.Input)
		}
	}
	if cfg.NodeID != "" {
		t.Fatalf("cfg.NodeID = %q on the per-org path", cfg.NodeID)
	}

	// a platform that takes the owner but answers nothing yet: one
	// registration with the owner, then today's per org
	f = seed(t)
	gw = gateway.New(f.URL(), "rs_test")
	cfg = &Config{Slug: "box", Name: "Box", OwnerID: ownerID, Capacity: Fixed(1)}
	scopes, _, err = RegisterOnce(context.Background(), gw, cfg)
	if err != nil || len(scopes) != 1 || cfg.NodeID != "" {
		t.Fatalf("scopes = %+v, node id %q, %v", scopes, cfg.NodeID, err)
	}
	if want, _ := cfg.NodeIDIn(orgID); scopes[0].NodeID != want || len(f.CallsNamed("registerNode")) != 2 {
		t.Fatalf("scope %+v, %d registrations", scopes[0], len(f.CallsNamed("registerNode")))
	}
}

// The node's one id: node.yaml's when it has one, else from (owner,
// slug), the same every time.
func TestOwnNodeID(t *testing.T) {
	kept, err := (&Config{NodeID: "n-1", OwnerID: ownerID, Slug: "mini"}).OwnNodeID()
	if err != nil || kept != "n-1" {
		t.Fatalf("kept = %q, %v", kept, err)
	}
	a, _ := (&Config{OwnerID: ownerID, Slug: "mini"}).OwnNodeID()
	b, _ := (&Config{OwnerID: ownerID, Slug: "mini"}).OwnNodeID()
	c, _ := (&Config{OwnerID: ownerID, Slug: "other"}).OwnNodeID()
	if a == "" || a != b || a == c {
		t.Fatalf("derived ids %q %q %q", a, b, c)
	}
	if _, err := (&Config{Slug: "mini"}).OwnNodeID(); err == nil {
		t.Fatal("no owner and no node_id must refuse")
	}
}

// node.yaml's owner is a person or an enterprise; anything else is refused.
func TestLoadConfigOwner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: Mini\nowner: enterprise\n")
	cfg, err := LoadConfig()
	if err != nil || cfg.OwnerKind() != OwnerEnterprise {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	writeNodeYAML(t, "slug: mini\nname: Mini\n")
	if cfg, err := LoadConfig(); err != nil || cfg.OwnerKind() != OwnerPerson {
		t.Fatalf("default owner = %v, %v", cfg, err)
	}
	writeNodeYAML(t, "slug: mini\nname: Mini\nowner: team\n")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), `owner "team"`) {
		t.Fatalf("err = %v", err)
	}
}

// The derived id is written back into node.yaml, the rest of it kept.
func TestWriteNodeIDKeepsTheFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "# the mini\nslug: mini\nname: Mini\ncapacity:\n  min: 1\n  max: 3\n")
	if err := WriteNodeID(answerNode); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil || cfg.NodeID != answerNode || cfg.Capacity.Max != 3 {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	path, _ := ConfigPath()
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# the mini") {
		t.Fatalf("comment lost:\n%s", data)
	}
	if err := WriteNodeID("other"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := LoadConfig(); cfg.NodeID != "other" {
		t.Fatalf("node_id = %q after a second write", cfg.NodeID)
	}
}
