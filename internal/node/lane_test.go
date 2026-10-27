package node

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The per-kind lanes (runsheet ADR 069 decision 2b) are retired: claims
// come from Mesh's priority queue, which ranks validations first, so no
// slot is kept from builds. node.yaml's capacity.reserve is still read
// without error — whatever it says — ignored, and said once.

// capacity.reserve parses, with any values, and is ignored: the
// capacity is its min and max, and the reserve is not written back.
func TestCapacityRetiredReserveIsReadAndIgnored(t *testing.T) {
	for _, src := range []string{
		"{min: 12, max: 12, reserve: {validate: 2, evaluate: 1}}",
		"{min: 2, max: 2, reserve: {validate: 5, evaluate: -1}}",
		"{min: 1, max: 4, reserve: {}}",
	} {
		var c Capacity
		if err := yaml.Unmarshal([]byte(src), &c); err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if !c.ReserveRetired() {
			t.Fatalf("%s: the retired reserve is not noted", src)
		}
		out, err := yaml.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "reserve") {
			t.Fatalf("%s: written back as %s", src, out)
		}
	}
	for _, src := range []string{"3", "{min: 1, max: 4}"} {
		var c Capacity
		if err := yaml.Unmarshal([]byte(src), &c); err != nil || c.ReserveRetired() {
			t.Fatalf("%s: %+v, %v", src, c, err)
		}
	}
}

// LoadConfig reads a node.yaml that still names capacity.reserve.
func TestCapacityRetiredReserveLoads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: {min: 12, max: 12, reserve: {validate: 2, evaluate: 1}}\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity.IsRange() || cfg.Capacity.Max != 12 || !cfg.Capacity.ReserveRetired() {
		t.Fatalf("capacity loaded as %+v", cfg.Capacity)
	}
}

// unlaned is a fixed capacity of n: every capacity keeps no lane now;
// the name is kept for the tests that count claim order and shares
// across every slot.
func unlaned(n int) Capacity { return Fixed(n) }

// fillBuilds marks n builds running on loop, as claimed builds are.
func fillBuilds(loop *Loop, n int) {
	loop.mu.Lock()
	defer loop.mu.Unlock()
	if loop.running == nil {
		loop.running = map[string]bool{}
	}
	for i := range n {
		loop.running[strings.Repeat("f", 8)+"-busy-"+string(rune('a'+i))] = true
	}
}

// No slot is kept from builds: at capacity 4 with three builds running
// a ready item is claimed in the fourth, and the holding summary names
// no lane.
func TestNoSlotIsKeptFromBuilds(t *testing.T) {
	loop, f, _, logs := batchLoop(t, oneStepYML("true"), 4)
	f.WorkItems[item1]["status"] = "ready"
	f.MergeBatches = map[string]map[string]any{}
	fillBuilds(loop, 3)
	tick(loop)
	if len(f.CallsNamed("claimWorkItem")) == 0 {
		t.Errorf("three builds running at capacity 4: the ready item was not claimed\n%s", logs.String())
	}
	if s := loop.Holding().Summary; strings.Contains(s, "lanes") {
		t.Errorf("the holding summary names a lane: %q", s)
	}
}

// A node whose node.yaml names capacity.reserve says once that it is
// ignored, however many ticks it runs.
func TestRetiredReserveSaidOnce(t *testing.T) {
	loop, _, _, logs := batchLoop(t, oneStepYML("true"), 4)
	loop.Cfg.Capacity.reserveNamed = true
	tick(loop)
	tick(loop)
	if n := strings.Count(logs.String(), "capacity.reserve is retired"); n != 1 {
		t.Fatalf("said %d times, want once\n%s", n, logs.String())
	}
}
