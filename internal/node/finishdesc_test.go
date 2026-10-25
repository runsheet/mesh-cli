package node

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
)

// FinishDescription's `## Who` section names the builder's actor, model
// and adapter, one `Paired with` line per pairer, and leaves the
// evaluator to the evaluation's own actor (every-actor-has-a-name item
// 3).
func TestFinishDescription(t *testing.T) {
	got := FinishDescription(FinishWho{
		Actor: "agent:builder@mini", Model: "opus", Adapter: "claude-code",
		Pairers: []Pairer{
			{Name: "Chris Kolenko", Email: "chris@example.com"},
			{Name: "Ann", Email: "ann@example.com"},
		},
	})
	want := "## Who\n\n" +
		"Built by agent:builder@mini on opus (claude-code)\n" +
		"Paired with Chris Kolenko <chris@example.com>\n" +
		"Paired with Ann <ann@example.com>\n" +
		"\n" +
		"Evaluated by: recorded by the evaluation's actor\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// runFinishDesc runs item1 on node slug mini with pair as node.yaml's
// pair: and, unless noDescription, a fake whose FinishWorkItemInput takes
// a description (Fake.TakesDescription), and returns the fake's finishWorkItem inputs.
func runFinishDesc(t *testing.T, pair []Pairer, noDescription bool, result harness.Result) []map[string]any {
	t.Helper()
	f := seed(t)
	f.TakesDescription = !noDescription
	r, _, item, _ := runnerOnFake(t, f, &resultHarness{result: result}, nil)
	r.Cfg.Slug, r.Cfg.Pair = "mini", pair
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatalf("RunItem: %v", err)
	}
	var finishes []map[string]any
	for _, c := range f.CallsNamed("finishWorkItem") {
		finishes = append(finishes, c.Input)
	}
	return finishes
}

// A build's finish carries the PR body FinishDescription renders: who
// built the item and who it was paired with (every-actor-has-a-name item
// 3b).
func TestRunnerFinishCarriesTheDescription(t *testing.T) {
	chris := Pairer{Name: "Chris Kolenko", Email: "chris@example.com"}
	finishes := runFinishDesc(t, []Pairer{chris}, false, harness.Result{Success: true, WorkCalls: map[string]map[string]any{
		"mesh_finish": {"item_id": item1, "branch": "mesh/g-1", "head_sha": "0000000"},
	}})
	if len(finishes) != 1 {
		t.Fatalf("finishWorkItem calls = %d, want 1", len(finishes))
	}
	desc, _ := finishes[0]["description"].(string)
	if !strings.Contains(desc, "Built by agent:builder@mini") {
		t.Fatalf("description = %q, want it to name agent:builder@mini", desc)
	}
	if !strings.Contains(desc, "Paired with Chris Kolenko <chris@example.com>") {
		t.Fatalf("description = %q, want it to name the pairer", desc)
	}
}

// A platform whose FinishWorkItemInput takes no description (runsheet's,
// the fake's default) is sent none — the key is absent, not empty — and the
// finish still succeeds.
func TestRunnerFinishWithNoDescriptionFieldOmitsIt(t *testing.T) {
	finishes := runFinishDesc(t, nil, true, harness.Result{Success: true, WorkCalls: map[string]map[string]any{
		"mesh_finish": {"item_id": item1, "branch": "mesh/g-1", "head_sha": "0000000"},
	}})
	if len(finishes) != 1 {
		t.Fatalf("finishWorkItem calls = %d, want 1", len(finishes))
	}
	if _, ok := finishes[0]["description"]; ok {
		t.Fatalf("finishWorkItem input = %v, want no description key", finishes[0])
	}
}

// An already_done finish carries the same description as a pushed one.
func TestRunnerAlreadyDoneFinishCarriesTheDescription(t *testing.T) {
	chris := Pairer{Name: "Chris Kolenko", Email: "chris@example.com"}
	ev := "https://github.com/acme/widgets/pull/3 merged it"
	finishes := runFinishDesc(t, []Pairer{chris}, false, harness.Result{Success: true, Text: "Done.", WorkCalls: map[string]map[string]any{
		"mesh_finish": {"item_id": item1, "outcome": "already_done", "evidence": ev},
	}})
	if len(finishes) != 1 || finishes[0]["outcome"] != "already_done" {
		t.Fatalf("finishWorkItem = %v, want one already_done finish", finishes)
	}
	desc, _ := finishes[0]["description"].(string)
	if !strings.Contains(desc, "Built by agent:builder@mini") || !strings.Contains(desc, "Paired with Chris Kolenko <chris@example.com>") {
		t.Fatalf("already_done description = %q", desc)
	}
}

// With no pairer the section says so, rather than naming nobody.
func TestFinishDescriptionWithNoPairer(t *testing.T) {
	got := FinishDescription(FinishWho{Actor: "agent:builder@mini", Model: "sonnet", Adapter: "claude-code"})
	want := "## Who\n\n" +
		"Built by agent:builder@mini on sonnet (claude-code)\n" +
		"Paired with nobody\n" +
		"\n" +
		"Evaluated by: recorded by the evaluation's actor\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
