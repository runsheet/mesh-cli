package harness

import (
	"strings"
	"testing"
)

// A mesh_verdict call reads as the text path's verdict: its readings
// as READING lines on the notes, its clauses with both halves and no
// duplicates, and a call with no readable passed is no verdict.
func TestVerdictCall(t *testing.T) {
	passed, notes, clauses, ok := VerdictCall(map[string]any{
		"passed": false, "notes": "Two gaps.",
		"failed_clauses": []any{
			map[string]any{"clause": "a", "satisfy": "do a"},
			map[string]any{"clause": "a", "satisfy": "again"},
			map[string]any{"clause": "b"},
		},
		"readings": `[{"clause": "under a minute", "reading": "48s"}]`,
	})
	if !ok || passed || len(clauses) != 1 || clauses[0] != (FailedClause{Clause: "a", Satisfy: "do a"}) {
		t.Fatalf("(%v, %v, %v)", passed, clauses, ok)
	}
	if notes != "Two gaps.\n\nREADING: under a minute || 48s" || len(ParseReadings(notes)) != 1 {
		t.Fatalf("notes %q", notes)
	}
	// no failed_clauses: the notes' FAILED lines are read
	_, _, clauses, _ = VerdictCall(map[string]any{"passed": "false", "notes": "FAILED: c || SATISFY: do c"})
	if len(clauses) != 1 || clauses[0].Clause != "c" {
		t.Fatalf("clauses from notes %v", clauses)
	}
	if _, _, _, ok := VerdictCall(map[string]any{"notes": "no passed"}); ok {
		t.Fatal("a call with no passed read as a verdict")
	}
}

// A mesh_plan call renders as the fenced block ParsePlan and its
// siblings read; one with neither items nor split is no plan.
func TestPlanCall(t *testing.T) {
	text, ok := PlanCall(map[string]any{
		"goal_id":   "g",
		"items":     []any{map[string]any{"seq": 1, "title": "T", "repo": "o/r", "depends_on": []any{}}},
		"questions": `[{"clause": "c", "question": "q?"}]`,
	})
	if !ok || !strings.HasPrefix(text, "```json\n") {
		t.Fatalf("(%q, %v)", text, ok)
	}
	specs, err := ParsePlan(text)
	if err != nil || len(specs) != 1 || specs[0].Title != "T" {
		t.Fatalf("ParsePlan: %v %v", specs, err)
	}
	if q := ParsePlanQuestions(text); len(q) != 1 || q[0].Question != "q?" {
		t.Fatalf("questions %v", q)
	}
	if _, ok := PlanCall(map[string]any{"goal_id": "g"}); ok {
		t.Fatal("a call with no items read as a plan")
	}
	if _, ok := PlanCall(nil); ok {
		t.Fatal("no call read as a plan")
	}
}
