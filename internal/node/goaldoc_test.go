package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

func TestRenderGoalDoc(t *testing.T) {
	d := GoalDoc{
		Slug: "ship-it", Title: "Ship it", Statement: "Make the thing.", Acceptance: "go test ./... passes",
		GoalStatus: "running", Parent: "prd",
		Items: []gateway.WorkItem{
			{Seq: 1, Title: "Write the | test", Status: "merged", Branch: "mesh/ship-it-1", PRURL: "https://github.com/acme/w/pull/1", CostCents: 300},
			{Seq: 2, Title: "Make it pass", Status: "running", CostCents: 0},
		},
	}
	at := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	first := RenderGoalDoc("", d, LogLine(at, 2, "started  the\nimplementation"))
	for _, want := range []string{
		"---\ntitle: Goal — Ship it\nkind: plan\nstatus: draft\nparent: prd\n---\n",
		"## Statement\n\nMake the thing.", "## Acceptance\n\ngo test ./... passes",
		"| 1 | Write the \\| test | merged | mesh/ship-it-1 | https://github.com/acme/w/pull/1 | $3.00 |",
		"| 2 | Make it pass | running | — | — | $0.00 |",
		"## Log\n\n- 2026-09-19 · item 2 · started the implementation\n",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("doc lacks %q:\n%s", want, first)
		}
	}

	// the same line again is not appended; the table is regenerated
	d.Items[1].Status = "review"
	d.Items[1].Branch = "mesh/ship-it-2"
	again := RenderGoalDoc(first, d, LogLine(at, 2, "started the implementation"))
	if strings.Count(again, "· item 2 · started the implementation") != 1 {
		t.Fatalf("log line duplicated:\n%s", again)
	}
	if !strings.Contains(again, "| 2 | Make it pass | review | mesh/ship-it-2 |") || strings.Contains(again, "| 2 | Make it pass | running |") {
		t.Fatalf("table not regenerated:\n%s", again)
	}
	// a new line appends after the old
	d.GoalStatus = "done"
	third := RenderGoalDoc(again, d, LogLine(at.Add(24*time.Hour), 2, "finished"))
	if !strings.Contains(third, "- 2026-09-19 · item 2 · started the implementation\n- 2026-09-20 · item 2 · finished\n") {
		t.Fatalf("log did not append:\n%s", third)
	}
	if !strings.Contains(third, "status: approved\n") {
		t.Fatal("done goal must render status: approved")
	}
	if !strings.Contains(RenderGoalDoc("", GoalDoc{Slug: "x", GoalStatus: "cancelled"}, ""), "status: superseded\n") {
		t.Fatal("cancelled goal must render status: superseded")
	}
	if strings.Contains(RenderGoalDoc("", GoalDoc{Slug: "x"}, ""), "parent:") {
		t.Fatal("no parent must omit the key")
	}
}

func TestWriteGoalDoc(t *testing.T) {
	ws := t.TempDir()
	d := GoalDoc{Slug: "ship-it", Title: "Ship it", GoalStatus: "running"}
	rel, changed, err := WriteGoalDoc(ws, d, "- 2026-09-19 · item 1 · hello")
	if err != nil || !changed || rel != filepath.Join("specs", "goal-ship-it.md") {
		t.Fatalf("first write: rel=%q changed=%v err=%v", rel, changed, err)
	}
	if _, changed, err := WriteGoalDoc(ws, d, "- 2026-09-19 · item 1 · hello"); err != nil || changed {
		t.Fatalf("second identical write must not change: changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(filepath.Join(ws, "specs", "goal-ship-it.md"))
	if !strings.HasPrefix(string(b), "---\ntitle: Goal — Ship it\n") {
		t.Fatalf("file:\n%s", b)
	}
}
