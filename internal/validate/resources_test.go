package validate

import (
	"strings"
	"testing"
)

// A suite's cpus and memory are parsed, refused when they are no size,
// and never part of the suite hash.
func TestASuiteNamesItsOwnRoom(t *testing.T) {
	const plain = "suites:\n  - name: integration\n    kind: integration\n    command: go test ./e2e\n"
	cfg, err := Parse([]byte(plain + "    cpus: 8\n    memory: 24g\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Steps[0].Resources(); got != (Resources{CPUs: 8, Memory: "24g"}) || got.MemoryBytes() != 24<<30 {
		t.Fatalf("the suite asks for %+v, want 8 cpus and 24g", got)
	}
	bare, err := Parse([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	if !bare.Steps[0].Resources().IsZero() {
		t.Fatalf("a suite naming no size asks for %+v", bare.Steps[0].Resources())
	}
	if cfg.Hash() != bare.Hash() {
		t.Fatal("a suite's room changed its hash")
	}
	for bad, want := range map[string]string{"    cpus: -1\n": "cpus", "    memory: lots\n": "memory"} {
		if _, err := Parse([]byte(plain + bad)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q parsed: %v", bad, err)
		}
	}
}

// A shard's lane asks for its suite's room, a group's lane the most any
// of its suites asks, and a lane naming none asks for nothing.
func TestLaneSizesFollowTheirSuites(t *testing.T) {
	steps := []Step{
		{Name: "vet", Kind: "lint", Group: "fast", Command: "true", CPUs: 1, Memory: "512m"},
		{Name: "unit", Kind: "unit", Group: "fast", Command: "true", CPUs: 6, Memory: "2g"},
		{Name: "integration", Kind: "integration", Command: "true", Shards: 2, CPUs: 8, Memory: "24g"},
		{Name: "build", Kind: "build", Command: "true"},
	}
	got := LaneSizes(steps)
	want := []LaneSize{
		{Name: "fast", Resources: Resources{CPUs: 6, Memory: "2g"}},
		{Name: "integration-0", Resources: Resources{CPUs: 8, Memory: "24g"}},
		{Name: "integration-1", Resources: Resources{CPUs: 8, Memory: "24g"}},
		{Name: "build"},
	}
	if len(got) != len(want) || len(got) != Lanes(steps) {
		t.Fatalf("lanes %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lane %d is %+v, want %+v", i, got[i], want[i])
		}
	}
}
