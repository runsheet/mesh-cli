package harness

import (
	"fmt"
	"os"
	"path/filepath"
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
		"7. " + evaluateRetryRule,
		"9. End your final message",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("a retry's evaluate prompt lacks %q", want)
		}
	}
	// a first attempt is told none of it, and its verdict rule stays rule 7
	first := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main"})
	for _, never := range []string{"# What earlier attempts failed on", evaluateRetryRule, "9. End your final message"} {
		if strings.Contains(first, never) {
			t.Errorf("a first attempt's evaluate prompt carries %q", never)
		}
	}
	if !strings.Contains(first, "7. End your final message") {
		t.Error("a first attempt's verdict rule is not rule 7")
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

// Every evaluator, Claude or Codex, is told to judge a host reading on
// its mechanism and record it, with ci-fits-the-mini as the example; a
// retry's is told to open with the same-clause line (ADR-020).
func TestEvaluatePromptJudgesWhatAPRCanChange(t *testing.T) {
	base := EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main"}
	first := EvaluatePrompt(base)
	for _, want := range []string{
		"6. " + evaluateHostRule + "\n",
		"A clause whose truth depends on the host you run on",
		"judged on its mechanism",
		"`READING: <the clause, as the acceptance words it> || <what you measured, when, under what load>`",
		"never because the host's number is over the threshold",
		"`the three shards' durations within 20% of each other`",
		"load ran 30–65 on 18 cores",
		"1.09, 1.13 and 1.19",
		"all 309 tests",
		"item 14 fails on that stale file — not on its 1.09",
		"7. End your final message",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("a first attempt's evaluate prompt lacks %q", want)
		}
	}
	for _, never := range []string{SameClauseLine, "8. ", "9. "} {
		if strings.Contains(first, never) {
			t.Errorf("a first attempt's evaluate prompt carries %q", never)
		}
	}
	retry := base
	retry.PriorAttempts = []PriorAttempt{{Attempt: 1, Clauses: []FailedClause{{"a", "b"}}}}
	p := EvaluatePrompt(retry)
	for _, want := range []string{
		"6. " + evaluateHostRule + "\n",
		"7. " + evaluateRetryRule + "\n",
		"8. " + evaluateSameClauseRule + "\n",
		"the first line of your review must be `fails on the same clause as attempt N: <the clause>`",
		"9. End your final message",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("a retry's evaluate prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "10. ") {
		t.Error("a retry's evaluate prompt numbers past rule 9")
	}
}

func TestParseReadings(t *testing.T) {
	text := "review\n" +
		"READING: the three shards within 20% || 1.19 max/min at 03:10Z, load 41 on 18 cores\n" +
		"- **READING: `p90 under 1.5 × cores` || 3.6 × over 2026-09-29**\n" +
		"READING: no value here\n" +
		"READING: nothing measured ||\n" +
		"READING:  || a value with no clause\n" +
		"READING: the three shards within 20% || a second reading of it\n" +
		"VERDICT: PASS"
	want := []Reading{
		{"the three shards within 20%", "1.19 max/min at 03:10Z, load 41 on 18 cores"},
		{"p90 under 1.5 × cores", "3.6 × over 2026-09-29"},
	}
	got := ParseReadings(text)
	if len(got) != len(want) {
		t.Fatalf("%d readings, want %d: %+v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("reading %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got := ParseReadings("the branch is fine\nVERDICT: PASS"); got != nil {
		t.Errorf("readings from a verdict with none: %+v", got)
	}
}

// cifits reads a ci-fits-the-mini verdict kept under testdata (item 3
// reuses them) and its clauses.
func cifits(t *testing.T, item int) (string, []FailedClause) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", fmt.Sprintf("cifits_item%d_verdict.txt", item)))
	if err != nil {
		t.Fatal(err)
	}
	clauses := ParseFailedClauses(string(b))
	if len(clauses) == 0 {
		t.Fatalf("item %d's verdict names no clause", item)
	}
	return string(b), clauses
}

// Item 17 failed on the clause item 14 failed on, in other words: the
// same clause, which SameClause finds.
func TestSameClauseCIFitsTheMini(t *testing.T) {
	notes14, clauses14 := cifits(t, 14)
	_, clauses17 := cifits(t, 17)
	prior := []PriorAttempt{{Attempt: 1, Clauses: clauses14, Notes: notes14}}
	attempt, clause, ok := SameClause(prior, clauses17)
	if !ok || attempt != 1 || clause != clauses17[0].Clause {
		t.Fatalf("SameClause = %d, %q, %v; want attempt 1, %q", attempt, clause, ok, clauses17[0].Clause)
	}
	if _, _, ok := SameClause(prior, []FailedClause{{"go vet ./... is clean", "fix the vet warning"}}); ok {
		t.Error("an unrelated clause matched item 14's")
	}
	if _, _, ok := SameClause(nil, clauses17); ok {
		t.Error("a first attempt matched a prior clause")
	}
}

func TestSameClauseNormalises(t *testing.T) {
	prior := []PriorAttempt{
		{Attempt: 1, Clauses: []FailedClause{{"the `README` is updated.", "x"}}},
		{Attempt: 2, Clauses: []FailedClause{{"tests pass", "x"}}},
		{Attempt: 3, Clauses: []FailedClause{{"The README   is updated", "x"}}},
	}
	if a, _, ok := SameClause(prior, []FailedClause{{"the readme is updated;", "y"}}); !ok || a != 3 {
		t.Errorf("SameClause = %d, %v; want the latest attempt, 3", a, ok)
	}
}

func TestNoteSameClause(t *testing.T) {
	notes14, clauses14 := cifits(t, 14)
	notes17, clauses17 := cifits(t, 17)
	prior := []PriorAttempt{{Attempt: 1, Clauses: clauses14, Notes: notes14}}
	got := NoteSameClause(notes17, prior, clauses17)
	want := "fails on the same clause as attempt 1: the three shards within 20%\n\n" + notes17
	if got != want {
		t.Fatalf("NoteSameClause opens with %q", strings.SplitN(got, "\n", 2)[0])
	}
	if again := NoteSameClause(got, prior, clauses17); again != got {
		t.Error("NoteSameClause prepended the line twice")
	}
	own := "**Fails on the same clause as attempt 1: the shards within 20%**\n\nreview"
	if NoteSameClause(own, prior, clauses17) != own {
		t.Error("NoteSameClause rewrote notes that already open with the line")
	}
	unrelated := []FailedClause{{"go vet ./... is clean", "fix it"}}
	if NoteSameClause(notes17, prior, unrelated) != notes17 {
		t.Error("NoteSameClause changed notes that repeat no clause")
	}
}
