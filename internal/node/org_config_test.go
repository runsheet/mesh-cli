package node

import (
	"os"
	"strings"
	"testing"
)

// `owner: org` with `org: grapevine` loads cleanly: an org's own box.
func TestOrgConfigOwnerOrgWithOrgLoads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\nowner: org\norg: grapevine\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Owner != "org" || cfg.OrgLogin != "grapevine" {
		t.Fatalf("cfg = %+v, want owner=org, org=grapevine", cfg)
	}
	if cfg.OwnerKind() != "org" {
		t.Fatalf("OwnerKind() = %q, want org", cfg.OwnerKind())
	}
}

// `owner: org` names no org by itself: LoadConfig refuses it and names
// the fix.
func TestOrgConfigOwnerOrgWithoutOrgRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\nowner: org\n")
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "org: <slug or id>") {
		t.Fatalf("err = %v, want it to name `org: <slug or id>`", err)
	}
}

// `org:` set beside a non-org owner is refused, whatever the owner.
func TestOrgConfigOrgWithoutOwnerOrgRefused(t *testing.T) {
	for _, owner := range []string{"", "owner: person\n", "owner: enterprise\nenterprise: anythink\n"} {
		t.Setenv("HOME", t.TempDir())
		writeNodeYAML(t, "slug: mini\nname: mini\n"+owner+"org: grapevine\n")
		_, err := LoadConfig()
		if err == nil || !strings.Contains(err.Error(), "owner: is not org") {
			t.Fatalf("%q: err = %v, want it to say owner: is not org", owner, err)
		}
	}
}

// An owner kind the node does not know is refused, naming all three.
func TestOrgConfigUnknownOwnerKindRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\nowner: team\n")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("owner: team loaded, want it refused")
	}
	for _, kind := range []string{`"person"`, `"enterprise"`, `"org"`} {
		if !strings.Contains(err.Error(), kind) {
			t.Fatalf("err = %v, want it to name %s", err, kind)
		}
	}
}

// org_id is written by Save and read back by LoadConfig, as
// enterprise_id is.
func TestOrgConfigOrgIDRoundTrips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\nowner: org\norg: grapevine\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OrgID = "0b6f1c0e-0000-4000-8000-00000000a0a0"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	path, _ := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "org_id: "+cfg.OrgID) {
		t.Fatalf("node.yaml =\n%s\nwant org_id: %s", data, cfg.OrgID)
	}
	again, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if again.OrgID != cfg.OrgID || again.OrgLogin != "grapevine" || again.OwnerKind() != "org" {
		t.Fatalf("reloaded = %+v, want org grapevine, org_id %s", again, cfg.OrgID)
	}
}

// The other kinds keep their OwnerKind.
func TestOwnerKindOfEachOwner(t *testing.T) {
	for owner, want := range map[string]string{"": "person", "person": "person", "enterprise": "enterprise", "org": "org"} {
		if got := (&Config{Owner: owner}).OwnerKind(); got != want {
			t.Errorf("OwnerKind(%q) = %q, want %q", owner, got, want)
		}
	}
}

// ValidateOwner refuses owner: org with a blank org: too.
func TestValidateOwnerOrgBlankRefused(t *testing.T) {
	err := (&Config{Owner: "org", OrgLogin: "  "}).ValidateOwner()
	if err == nil || !strings.Contains(err.Error(), "e.g. org: grapevine") {
		t.Fatalf("err = %v, want the example org: grapevine", err)
	}
	if err := (&Config{Owner: "org", OrgLogin: "grapevine"}).ValidateOwner(); err != nil {
		t.Fatalf("owner: org, org: grapevine: %v", err)
	}
}

// An org owner is named with its kind, as an enterprise is.
func TestOrgConfigOwnerWhoNamesAnOrg(t *testing.T) {
	if got := (Owner{Kind: "org", Name: "Grapevine"}).Who(); got != "Grapevine (org)" {
		t.Fatalf("Who() = %q, want Grapevine (org)", got)
	}
	if got := (Owner{Kind: "enterprise", Name: "anythink"}).Who(); got != "anythink (enterprise)" {
		t.Fatalf("Who() = %q, want anythink (enterprise)", got)
	}
}

// An older single-org registration — org:, org_id: and account_id: as
// the per-org register wrote them, no owner: — still loads, read as
// Orgs of one; owner: org's org: is not read that way.
func TestOrgConfigLegacySingleOrgRegistrationStillLoads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\norg: acme\norg_id: 11111111-1111-4111-8111-111111111111\naccount_id: 33333333-3333-4333-8333-333333333333\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OwnerKind() != "person" || len(cfg.Orgs) != 1 || cfg.Orgs[0] != "acme" {
		t.Fatalf("cfg = %+v, want a person's node serving acme", cfg)
	}
	writeNodeYAML(t, "slug: mini\nname: mini\nowner: org\norg: grapevine\n")
	if cfg, err = LoadConfig(); err != nil || len(cfg.Orgs) != 0 {
		t.Fatalf("owner: org: cfg.Orgs = %v, err = %v; want no orgs: restriction", cfg.Orgs, err)
	}
}
