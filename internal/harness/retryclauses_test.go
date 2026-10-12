package harness

import (
	"strings"
	"testing"
)

// A retry is handed the clauses the last evaluation failed, as a list, not
// only the prose of its review: the structured record is the only channel
// between one attempt and the next.
func TestBuildPromptCarriesTheFailedClauses(t *testing.T) {
	in := PromptInput{
		GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main",
		Attempts: 2, Evaluation: "Review: FAIL. The e2e test skips.",
		FailedClauses: []FailedClause{
			{"the e2e test runs", "TestRigger does not skip before the flow"},
			{"the trd is amended", "specs/trd.md §6a describes the process"},
		},
	}
	p := BuildPrompt(in)
	for _, want := range []string{
		"## Previous attempt",
		"This is attempt 3.",
		"Review: FAIL. The e2e test skips.",
		"### Every clause it failed",
		"- **the e2e test runs** — satisfied by: TestRigger does not skip before the flow",
		"- **the trd is amended** — satisfied by: specs/trd.md §6a describes the process",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("build prompt lacks %q", want)
		}
	}
}

// An evaluator that named no clauses leaves the prompt as it was.
func TestBuildPromptWithoutClausesIsUnchanged(t *testing.T) {
	base := PromptInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main",
		Attempts: 1, Evaluation: "Review: FAIL. Something."}
	p := BuildPrompt(base)
	if strings.Contains(p, "Every clause it failed") {
		t.Error("a retry with no clauses got the clause section")
	}
	for _, want := range []string{"## Previous attempt", "This is attempt 2.", "Review: FAIL. Something."} {
		if !strings.Contains(p, want) {
			t.Errorf("build prompt lacks %q", want)
		}
	}
	// a first attempt is told none of it
	first := base
	first.Attempts, first.Evaluation = 0, ""
	if p := BuildPrompt(first); strings.Contains(p, "Previous attempt") || strings.Contains(p, "Every clause it failed") {
		t.Error("a first attempt carries a previous-attempt section")
	}
}

// The planner plans no goal document: goals keep none in the repository
// (retired 2026-10-12), so an item that writes one is waste.
func TestPlanPromptPlansNoGoalDocItem(t *testing.T) {
	p := PlanPrompt(PlanInput{GoalSlug: "g", GoalTitle: "G", Statement: "s", Acceptance: "a",
		Repos: map[string]string{"o/r": "r"}})
	if !strings.Contains(p, planGoalDocRule) {
		t.Error("the plan prompt does not keep the planner out of the goal document")
	}
	for _, gone := range []string{"a goal-doc section", "A final docs or goal-doc item"} {
		if strings.Contains(p, gone) {
			t.Errorf("the plan prompt still invites a goal-doc item: %q", gone)
		}
	}
	if !strings.Contains(p, "goals keep no document in the repository") {
		t.Error("the rule does not say goals keep no document")
	}
}
