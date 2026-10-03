package node

import (
	"strings"
	"testing"
)

// `owner: enterprise` names no enterprise by itself: LoadConfig refuses
// it and names the fix.
func TestLoadConfigOwnerEnterpriseWithoutEnterpriseRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\nowner: enterprise\n")
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "enterprise: <slug or id>") {
		t.Fatalf("err = %v, want it to name `enterprise: <slug or id>`", err)
	}
}

// `enterprise:` set beside a non-enterprise owner is refused too.
func TestLoadConfigEnterpriseWithoutOwnerEnterpriseRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\nowner: person\nenterprise: anythink\n")
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "owner: is not enterprise") {
		t.Fatalf("err = %v, want it to say owner: is not enterprise", err)
	}
}

// `enterprise:` with no `owner:` at all (empty is "person") is refused
// the same way.
func TestLoadConfigEnterpriseWithNoOwnerRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\nenterprise: anythink\n")
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "owner: is not enterprise") {
		t.Fatalf("err = %v, want it to say owner: is not enterprise", err)
	}
}

// `owner: enterprise` with `enterprise: anythink` loads cleanly.
func TestLoadConfigOwnerEnterpriseWithEnterpriseLoads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\nowner: enterprise\nenterprise: anythink\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Owner != "enterprise" || cfg.Enterprise != "anythink" {
		t.Fatalf("cfg = %+v, want owner=enterprise, enterprise=anythink", cfg)
	}
	if cfg.OwnerKind() != "enterprise" {
		t.Fatalf("OwnerKind() = %q, want enterprise", cfg.OwnerKind())
	}
}

// `owner: person` alone, with no `enterprise:`, still loads — the
// ordinary bring-your-own-infra case.
func TestLoadConfigOwnerPersonAloneLoads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\nowner: person\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Owner != "person" || cfg.Enterprise != "" {
		t.Fatalf("cfg = %+v, want owner=person, enterprise empty", cfg)
	}
}

// No `owner:` and no `enterprise:` at all still loads, as it always
// has.
func TestLoadConfigNoOwnerNoEnterpriseLoads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Owner != "" || cfg.Enterprise != "" {
		t.Fatalf("cfg = %+v, want both empty", cfg)
	}
}
