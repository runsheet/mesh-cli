package nodetools

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/validate"
)

// run_suite's shards starts only the first k of a sharded suite's
// shards, and only for a selection; none, or a whole run, starts every
// one.
func TestRunSuiteShardsStartsTheFirstShards(t *testing.T) {
	steps := []validate.Step{{Name: "unit"}, {Name: "integration", Shards: 4}}
	sel := &Selection{Targets: []string{"e2e:TestA"}}
	got := startShards(steps, 1, sel, nil)
	if got[0].RunShards != 0 || got[1].RunShards != 1 {
		t.Fatalf("shards 1 with a selection: %+v", got)
	}
	if steps[1].RunShards != 0 {
		t.Fatal("startShards changed the caller's steps")
	}
	if got := startShards(steps, 1, nil, nil); got[1].RunShards != 0 {
		t.Fatal("a whole run started fewer than every shard")
	}
	if got := startShards(steps, 9, sel, nil); got[1].RunShards != 0 {
		t.Fatal("shards past the declared count did not start every shard")
	}
	for _, bad := range []any{0.0, 1.5, "2"} {
		if _, err := shardsArg(map[string]any{"shards": bad}); err == nil {
			t.Fatalf("shards %v was taken", bad)
		}
	}
	if k, err := shardsArg(map[string]any{"shards": 2.0}); err != nil || k != 2 {
		t.Fatalf("shards 2: %d, %v", k, err)
	}
}
