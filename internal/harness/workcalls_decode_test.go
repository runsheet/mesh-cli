package harness

import "testing"

// A mesh_result verdict reads as the node records it: its readings as
// READING lines on the notes, its clauses (failedClauses, as finishJob
// names them, or failed_clauses) with both halves and no duplicates, and
// a verdict with no readable passed is no verdict.
func TestVerdictResult(t *testing.T) {
	passed, notes, clauses, ok := VerdictResult(map[string]any{
		"passed": false, "notes": "Two gaps.",
		"failedClauses": []any{
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
	_, _, clauses, _ = VerdictResult(map[string]any{"passed": "false", "failed_clauses": []any{map[string]any{"clause": "c", "satisfy": "do c"}}})
	if len(clauses) != 1 || clauses[0].Clause != "c" {
		t.Fatalf("snake_case clauses %v", clauses)
	}
	// no clauses in the call: none, whatever the notes say (the text is
	// not read, ADR-041)
	if _, _, clauses, _ = VerdictResult(map[string]any{"passed": "false", "notes": "FAILED: c || SATISFY: do c"}); len(clauses) != 0 {
		t.Fatalf("clauses read from the notes: %v", clauses)
	}
	if _, _, _, ok := VerdictResult(map[string]any{"notes": "no passed"}); ok {
		t.Fatal("a verdict with no passed read as one")
	}
}
