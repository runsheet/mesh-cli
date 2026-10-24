package node

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The lanes (runsheet ADR 069 decision 2b, goal
// a-node-keeps-a-lane-for-validations-and-evaluations): node.yaml's
// capacity.reserve keeps slots no build takes, for validations and
// evaluations; a node of capacity 4 or more keeps one of each unasked.

// capacity: parses as a number, a range, and a mapping with a reserve;
// the lanes in force are node.yaml's, else the default.
func TestCapacityReserveParses(t *testing.T) {
	for _, c := range []struct {
		yaml     string
		want     Capacity
		lanes    Reserve
		inForce  int
		set      bool
		saveBack string
	}{
		{"3", Fixed(3), Reserve{}, 3, false, "3\n"},
		{"12", Fixed(12), Reserve{Validate: 1, Evaluate: 1}, 12, false, "12\n"},
		{"{min: 1, max: 4}", Capacity{Min: 1, Max: 4}, Reserve{Validate: 1, Evaluate: 1}, 4, false, "min: 1\nmax: 4\n"},
		{"{min: 12, max: 12, reserve: {validate: 2, evaluate: 1}}", Capacity{Min: 12, Max: 12, Reserve: ReserveOf(2, 1)}, Reserve{Validate: 2, Evaluate: 1}, 12, true, "min: 12\nmax: 12\nreserve:\n    validate: 2\n    evaluate: 1\n"},
		{"{min: 4, max: 8, reserve: {validate: 0, evaluate: 0}}", Capacity{Min: 4, Max: 8, Reserve: ReserveOf(0, 0)}, Reserve{}, 8, true, "min: 4\nmax: 8\nreserve:\n    validate: 0\n    evaluate: 0\n"},
		{"{min: 2, max: 2, reserve: {validate: 1}}", Capacity{Min: 2, Max: 2, Reserve: ReserveOf(1, 0)}, Reserve{Validate: 1}, 2, true, "min: 2\nmax: 2\nreserve:\n    validate: 1\n    evaluate: 0\n"},
	} {
		t.Run(c.yaml, func(t *testing.T) {
			var got Capacity
			if err := yaml.Unmarshal([]byte(c.yaml), &got); err != nil {
				t.Fatal(err)
			}
			if got != c.want || got.Reserve.IsSet() != c.set {
				t.Fatalf("parsed %+v, want %+v", got, c.want)
			}
			l := got.Lanes(c.inForce)
			if l.Validate != c.lanes.Validate || l.Evaluate != c.lanes.Evaluate {
				t.Errorf("lanes at %d = %+v, want %+v", c.inForce, l, c.lanes)
			}
			out, err := yaml.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != c.saveBack {
				t.Errorf("saved back as %q, want %q", out, c.saveBack)
			}
		})
	}
}

// A reserve that is negative or leaves a build no slot is refused.
func TestCapacityReserveRejected(t *testing.T) {
	for _, c := range []string{
		"{min: 4, max: 4, reserve: {validate: -1}}",
		"{min: 2, max: 2, reserve: {validate: 1, evaluate: 1}}",
		"{min: 4, max: 4, reserve: [1, 1]}",
	} {
		t.Run(c, func(t *testing.T) {
			var got Capacity
			if err := yaml.Unmarshal([]byte(c), &got); err == nil {
				t.Fatalf("capacity %s parsed as %+v, want an error", c, got)
			}
		})
	}
}

// The default lanes follow the capacity in force: a ranged node scaled
// below ReserveFloor keeps none.
func TestCapacityDefaultLanesFollowTheCapacityInForce(t *testing.T) {
	c := Capacity{Min: 1, Max: 8}
	if got := c.Lanes(3); got.Total() != 0 {
		t.Errorf("lanes at 3 of 1–8 = %+v, want none", got)
	}
	if got := c.Lanes(4); got.Validate != 1 || got.Evaluate != 1 {
		t.Errorf("lanes at 4 of 1–8 = %+v, want 1 and 1", got)
	}
}

// The lanes always leave a build one slot of the capacity in force: a
// hot host's or a scaler's smaller capacity shrinks the evaluation lane
// first.
func TestCapacityLanesShrinkToTheCapacityInForce(t *testing.T) {
	c := Capacity{Min: 12, Max: 12, Reserve: ReserveOf(2, 2)}
	for inForce, want := range map[int]Reserve{12: {Validate: 2, Evaluate: 2}, 4: {Validate: 2, Evaluate: 1}, 2: {Validate: 1}, 1: {}, 0: {}} {
		if got := c.Lanes(inForce); got.Validate != want.Validate || got.Evaluate != want.Evaluate {
			t.Errorf("lanes at %d = %+v, want %+v", inForce, got, want)
		}
	}
}

// LoadConfig reads capacity.reserve from node.yaml.
func TestCapacityReserveLoads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: {min: 12, max: 12, reserve: {validate: 2, evaluate: 1}}\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity.IsRange() || cfg.Capacity.Max != 12 || cfg.Capacity.Reserve != ReserveOf(2, 1) {
		t.Fatalf("capacity loaded as %+v", cfg.Capacity)
	}
}

// unlaned is a fixed capacity of n that keeps no lanes, for the tests
// that count claim order and shares across every slot.
func unlaned(n int) Capacity {
	c := Fixed(n)
	c.Reserve = ReserveOf(0, 0)
	return c
}

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

// The dl360 fault of 2026-10-24: a node with every slot taken by builds
// still claims a waiting batch candidate within one tick, in its lane.
func TestLaneClaimsACandidateAtCapacity(t *testing.T) {
	loop, f, batch, logs := batchLoop(t, oneStepYML("true"), 4)
	fillBuilds(loop, 4)
	started, release := make(chan string, 1), make(chan struct{})
	loop.ValidateBatch = blockingBatch(started, release)
	loop.Tick(context.Background())
	claimed := false
	for _, c := range f.CallsNamed("claimMergeBatchValidation") {
		claimed = claimed || c.Input["aggregateId"] == batch.MergeBatchID
	}
	if !claimed {
		t.Fatalf("a node at capacity with builds did not claim the waiting candidate\n%s", logs.String())
	}
	<-started
	if !strings.Contains(loop.Holding().Summary, "lanes kept: 1 validation, 1 evaluation") {
		t.Errorf("the holding summary names no lane: %q", loop.Holding().Summary)
	}
	close(release)
	loop.wg.Wait()
}

// A node of capacity 1 keeps no lane: at capacity it claims no batch.
func TestLaneNoneBelowTheFloor(t *testing.T) {
	loop, f, _, _ := batchLoop(t, oneStepYML("true"), 1)
	fillBuilds(loop, 1)
	tick(loop)
	if n := len(f.CallsNamed("claimMergeBatchValidation")); n != 0 {
		t.Errorf("a full node of capacity 1 made %d batch claims", n)
	}
	if s := loop.Holding().Summary; strings.Contains(s, "lanes") {
		t.Errorf("the holding summary names a lane at capacity 1: %q", s)
	}
}

// A build never takes a lane: at capacity 4 with two builds running the
// two slots left are the lanes, and a ready item is not claimed; with
// one build running it is.
func TestLaneBuildsDoNotTakeTheReservedSlots(t *testing.T) {
	for _, c := range []struct {
		running int
		claims  bool
	}{{2, false}, {1, true}} {
		loop, f, _, logs := batchLoop(t, oneStepYML("true"), 4)
		f.WorkItems[item1]["status"] = "ready"
		f.MergeBatches = map[string]map[string]any{}
		fillBuilds(loop, c.running)
		tick(loop)
		got := len(f.CallsNamed("claimWorkItem")) > 0
		if got != c.claims {
			t.Errorf("%d build(s) running at capacity 4: claimed %v, want %v\n%s", c.running, got, c.claims, logs.String())
		}
	}
}
