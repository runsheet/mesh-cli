package harness

import (
	"fmt"
	"strings"
	"testing"
)

// A failing evaluator names each clause it failed and what would satisfy
// it, so the retry is told what to fix in a shape the platform stores.
func TestParseFailedClauses(t *testing.T) {
	for _, c := range []struct {
		name string
		text string
		want []FailedClause
	}{
		{"plain", "review\nFAILED: tests pass || SATISFY: TestThing covers the empty case\nVERDICT: FAIL",
			[]FailedClause{{"tests pass", "TestThing covers the empty case"}}},
		{"a list the evaluator decorated", "- **FAILED: the trd is amended || SATISFY: specs/trd.md §6a describes it**\n",
			[]FailedClause{{"the trd is amended", "specs/trd.md §6a describes it"}}},
		{"SATISFY is optional", "FAILED: vet is clean || go vet ./... exits 0\n",
			[]FailedClause{{"vet is clean", "go vet ./... exits 0"}}},
		{"two clauses keep their order", "FAILED: b || fix b\nFAILED: a || fix a\n",
			[]FailedClause{{"b", "fix b"}, {"a", "fix a"}}},
		{"no clauses", "the branch is fine\nVERDICT: PASS", nil},
		{"a clause with no separator is dropped", "FAILED: tests pass\nVERDICT: FAIL", nil},
		{"a clause with nothing that would satisfy it is dropped", "FAILED: tests pass ||\n", nil},
		{"a clause with no clause is dropped", "FAILED:  || fix it\n", nil},
		{"a duplicate clause is dropped", "FAILED: a || fix a\nFAILED: a || fix a differently\n",
			[]FailedClause{{"a", "fix a"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ParseFailedClauses(c.text)
			if len(got) != len(c.want) {
				t.Fatalf("%d clauses, want %d: %+v", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("clause %d = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

// An evaluator that lists rather than judges is capped.
func TestParseFailedClausesCapped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < ClauseMax+10; i++ {
		fmt.Fprintf(&b, "FAILED: clause %d || fix %d\n", i, i)
	}
	if got := ParseFailedClauses(b.String()); len(got) != ClauseMax {
		t.Fatalf("%d clauses, want the %d cap", len(got), ClauseMax)
	}
}

// A retry's evaluator is given what earlier attempts failed on and is
// pinned to it, so an item that has answered every objection already
// named is not failed on a fresh reading of the acceptance.
func TestEvaluatePromptPinsARetryToWhatFailedBefore(t *testing.T) {
	in := EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main",
		PriorAttempts: []PriorAttempt{
			{Attempt: 1, Notes: "the e2e test skips", Clauses: []FailedClause{{"the e2e test runs", "TestRigger does not skip"}}},
			{Attempt: 2, Notes: "two gaps left"},
		}}
	p := EvaluatePrompt(in)
	for _, want := range []string{
		"# What earlier attempts failed on",
		"## Attempt 1",
		"- the e2e test runs — satisfied by: TestRigger does not skip",
		"the e2e test skips",
		"## Attempt 2",
		"It named no clauses; its review is all there is.",
		"6. " + evaluateRetryRule,
		"7. End your final message",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("a retry's evaluate prompt lacks %q", want)
		}
	}
	// a first attempt is told none of it, and its verdict rule stays rule 6
	first := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main"})
	for _, never := range []string{"# What earlier attempts failed on", evaluateRetryRule, "7. End your final message"} {
		if strings.Contains(first, never) {
			t.Errorf("a first attempt's evaluate prompt carries %q", never)
		}
	}
	if !strings.Contains(first, "6. End your final message") {
		t.Error("a first attempt's verdict rule is not rule 6")
	}
}

// Both halves of the contract line reach every evaluator.
func TestEvaluatePromptAsksForTheClauseLines(t *testing.T) {
	p := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main"})
	for _, want := range []string{ClauseFailed, ClauseSatisfy, ClauseSep, "a clause that does not say what would satisfy it is dropped"} {
		if !strings.Contains(p, want) {
			t.Errorf("evaluate prompt lacks %q", want)
		}
	}
}
