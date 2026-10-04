package node

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Config parses `takes:` as a list or a per-kind cap map, refuses an
// unknown kind naming the five known ones, takes every kind when the
// key is left out, and Reloaded carries it.
func TestTakesConfigParsesAndReloads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// (a) a list: the named kind alone.
	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\ntakes: [evaluate]\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Takes.Takes(harness.KindEvaluate) {
		t.Fatalf("takes.Takes(evaluate) = false, want true")
	}
	if cfg.Takes.Takes(harness.KindBuild) {
		t.Fatalf("takes.Takes(build) = true, want false")
	}

	// (b) a mapping: the cap rides with the kind.
	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\ntakes: {evaluate: 2}\n")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := cfg.Takes.Cap(harness.KindEvaluate); !ok || n != 2 {
		t.Fatalf("takes.Cap(evaluate) = %d, %v, want 2, true", n, ok)
	}

	// (c) an unknown kind is refused, naming it and the known set.
	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\ntakes: [evaluation]\n")
	_, err = LoadConfig()
	if err == nil {
		t.Fatal("an unknown kind: LoadConfig did not err")
	}
	for _, want := range []string{"takes", "evaluation", "build", "evaluate", "rebase", "plan", "research"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to name %q", err, want)
		}
	}

	// (d) no takes: key takes every kind.
	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\n")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range KnownKinds {
		if !cfg.Takes.Takes(k) {
			t.Fatalf("with no takes:, Takes(%s) = false, want true", k)
		}
	}
	if got := cfg.Takes.Kinds(); len(got) != 5 {
		t.Fatalf("Kinds() = %v, want all five known kinds", got)
	}

	// (e) Reloader carries it.
	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\ntakes: [evaluate]\n")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	base := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", Capacity: Fixed(1)}
	reload := Reloader(gw, base, scopesOf(base), func() (*Config, error) { return cfg, nil }, func(context.Context, Scope) error { return nil })
	got, _ := reload(context.Background())
	if got.Takes == nil {
		t.Fatal("Reloaded.Takes is nil, want it set")
	}
	if !got.Takes.Takes(harness.KindEvaluate) {
		t.Fatalf("Reloaded.Takes.Takes(evaluate) = false, want true")
	}
	if got.Takes.Takes(harness.KindBuild) {
		t.Fatalf("Reloaded.Takes.Takes(build) = true, want false (evaluate alone)")
	}
}
