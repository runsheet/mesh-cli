package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	} {
		if !strings.Contains(p, want) {
			t.Errorf("a retry's evaluate prompt lacks %q", want)
		}
	}
	// a first attempt is told none of it, and its rules end at 6
	first := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main"})
	for _, never := range []string{"# What earlier attempts failed on", evaluateRetryRule, "7. "} {
		if strings.Contains(first, never) {
			t.Errorf("a first attempt's evaluate prompt carries %q", never)
		}
	}
}

// Both halves of a failed clause reach every evaluator, in the finish's
// account of `verdict.failedClauses` (ADR-041).
func TestEvaluatePromptAsksForTheClauseHalves(t *testing.T) {
	p := EvaluatePrompt(EvaluateInput{GoalSlug: "g", GoalTitle: "G", Seq: 1, Title: "T", Repo: "o/r", Branch: "b", BaseRef: "main"})
	for _, want := range []string{"`verdict.failedClauses` is one `{clause, satisfy}` per acceptance clause you fail", "a clause without both halves is dropped"} {
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
	} {
		if !strings.Contains(first, want) {
			t.Errorf("a first attempt's evaluate prompt lacks %q", want)
		}
	}
	for _, never := range []string{SameClauseLine, "7. ", "8. "} {
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
	} {
		if !strings.Contains(p, want) {
			t.Errorf("a retry's evaluate prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "9. ") {
		t.Error("a retry's evaluate prompt numbers past rule 8")
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

// failedClauseLines reads the FAILED/SATISFY lines of a verdict kept
// under testdata, the shape evaluations were recorded in before the
// result tool (ADR-041): the fixtures' clauses, for the same-clause
// tests.
func failedClauseLines(text string) []FailedClause {
	var out []FailedClause
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(strings.Trim(strings.TrimLeft(strings.TrimSpace(line), "-*\u2022># \t"), "`*_"))
		if !strings.HasPrefix(strings.ToUpper(l), ClauseFailed) {
			continue
		}
		clause, satisfy, ok := strings.Cut(l[len(ClauseFailed):], ClauseSep)
		if !ok {
			continue
		}
		satisfy = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(satisfy), ClauseSatisfy))
		if clause, satisfy = strings.TrimSpace(clause), strings.TrimSpace(satisfy); clause != "" && satisfy != "" {
			out = append(out, FailedClause{Clause: clause, Satisfy: satisfy})
		}
	}
	return out
}

// cifits reads a ci-fits-the-mini verdict kept under testdata (item 3
// reuses them) and its clauses.
func cifits(t *testing.T, item int) (string, []FailedClause) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", fmt.Sprintf("cifits_item%d_verdict.txt", item)))
	if err != nil {
		t.Fatal(err)
	}
	clauses := failedClauseLines(string(b))
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
