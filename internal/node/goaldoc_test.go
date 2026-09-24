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
		"---\ntitle: Goal — Ship it\nkind: plan\nstatus: draft\nsummary: Make the thing.\nparent: prd\n---\n",
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

// every document carries a summary, whatever the goal's status.
func TestRenderGoalDocAlwaysSummarises(t *testing.T) {
	for _, status := range []string{"", "running", "paused", "done", "cancelled"} {
		doc := RenderGoalDoc("", GoalDoc{Slug: "x", GoalStatus: status}, "")
		line := frontmatterValue(doc, "summary")
		if line == "" {
			t.Errorf("status %q: no summary line:\n%s", status, doc)
		}
	}
}

func TestSummaryOfFinishedGoal(t *testing.T) {
	d := GoalDoc{
		Slug: "ship-it", Title: "Ship it", GoalStatus: "done", SpentCents: 1234,
		Items: []gateway.WorkItem{
			{Seq: 1, Status: "merged"}, {Seq: 2, Status: "merged"}, {Seq: 3, Status: "failed"},
		},
	}
	const want = "2 of 3 items merged · $12.34 spent"
	if got := Summary(d); got != want {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
	doc := RenderGoalDoc("", d, "")
	if got := frontmatterValue(doc, "summary"); got != want {
		t.Errorf("frontmatter summary = %q, want %q", got, want)
	}
	if !strings.Contains(doc, "Mesh goal `ship-it`, done.\n\n**"+want+"**\n") {
		t.Errorf("body lacks the summary sentence:\n%s", doc)
	}
}

func TestSummaryOfRunningGoal(t *testing.T) {
	d := GoalDoc{Slug: "ship-it", Title: "Ship it", GoalStatus: "running", Statement: "Make the thing.\n\nThen make it fast.", SpentCents: 1234}
	if got := frontmatterValue(RenderGoalDoc("", d, ""), "summary"); got != "Make the thing." {
		t.Errorf("summary = %q, want the statement's first line", got)
	}
	d.GoalStatus = ""
	if got := frontmatterValue(RenderGoalDoc("", d, ""), "summary"); got != "Make the thing." {
		t.Errorf("no status: summary = %q, want the statement's first line", got)
	}
	d.Statement = ""
	if got := frontmatterValue(RenderGoalDoc("", d, ""), "summary"); got != "Ship it" {
		t.Errorf("no statement: summary = %q, want the title", got)
	}
	// a goal still running says nothing about how it ended
	if strings.Contains(RenderGoalDoc("", d, ""), "**Ship it**") {
		t.Error("a running goal must not carry the summary sentence in the body")
	}
}

// frontmatterValue reads one key out of a rendered document's frontmatter.
func frontmatterValue(doc, key string) string {
	_, rest, ok := strings.Cut(doc, "---\n")
	if !ok {
		return ""
	}
	fm, _, _ := strings.Cut(rest, "\n---\n")
	for _, l := range strings.Split(fm, "\n") {
		if v, ok := strings.CutPrefix(l, key+": "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
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

// A section the platform does not write — a builder's outcome, audit or
// list of secrets — survives every re-render, wherever it sat, whole.
func TestRenderGoalDocKeepsOtherSections(t *testing.T) {
	d := GoalDoc{Slug: "ship-it", Title: "Ship it", Statement: "Make the thing.", GoalStatus: "running",
		Items: []gateway.WorkItem{{Seq: 1, Title: "One", Status: "running"}}}
	at := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	first := RenderGoalDoc("", d, LogLine(at, 1, "started"))
	withBuilder := strings.Replace(first, "\n## Log\n", "\n## Secrets a person must add\n\n- `GATEWAY_TOKEN` — read-only\n- `CLOUDFLARE_API_TOKEN`\n  wrapped onto a second line\n\n### Why\n\nBecause.\n\n## Log\n", 1) +
		"\n## Outcome\n\n- **The five proposals** — moved into\n  the handbook.\n"
	d.Items[0].Status = "review"
	again := RenderGoalDoc(withBuilder, d, LogLine(at, 1, "finished"))
	for _, want := range []string{
		"## Secrets a person must add\n\n- `GATEWAY_TOKEN` — read-only\n- `CLOUDFLARE_API_TOKEN`\n  wrapped onto a second line\n\n### Why\n\nBecause.\n",
		"## Outcome\n\n- **The five proposals** — moved into\n  the handbook.\n",
		"| 1 | One | review |",
		"## Log\n\n- 2026-09-24 · item 1 · started\n- 2026-09-24 · item 1 · finished\n",
	} {
		if !strings.Contains(again, want) {
			t.Errorf("re-render lost %q:\n%s", want, again)
		}
	}
	if strings.Index(again, "## Outcome") > strings.Index(again, "## Log") {
		t.Errorf("kept sections go before the log:\n%s", again)
	}
	if strings.Count(again, "## Items") != 1 || strings.Count(again, "- 2026-09-24 · item 1 · started") != 1 {
		t.Errorf("owned sections duplicated:\n%s", again)
	}
	if third := RenderGoalDoc(again, d, LogLine(at, 1, "finished")); third != again {
		t.Errorf("re-render of an unchanged goal is not stable:\n%s\n---\n%s", again, third)
	}
}
