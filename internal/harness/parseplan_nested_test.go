package harness

import (
	"os"
	"testing"
)

// TestParsePlanNestedFence is the real message that three planner
// sessions produced on 2026-09-22 and the node threw away: a complete
// graph in a fenced block whose items mention a fence themselves, so
// the last marker in the message sits inside the JSON.
func TestParsePlanNestedFence(t *testing.T) {
	raw, err := os.ReadFile("testdata/plan_nested_fence.txt")
	if err != nil {
		t.Skip("fixture missing")
	}
	items, err := ParsePlan(string(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(items) < 2 {
		t.Fatalf("items: %d", len(items))
	}
	for _, it := range items {
		if it.Seq <= 0 || it.Title == "" || it.Repo == "" {
			t.Fatalf("bad item: %+v", it)
		}
	}
	t.Logf("parsed %d items", len(items))
}
