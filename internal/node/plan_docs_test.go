package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Goal items-that-share-a-file-are-planned-in-a-line: a goal's shared
// documents — the ADR, the TRD section, AGENTS.md, README — are
// assigned to one item; every other item's brief says not to touch
// them, and the writer's brief lists the items whose notes it folds in.

// docsItem is one fixture item: its seq, whether it is marked
// `writes_shared_docs`, and its dependencies (each with a because).
type docsItem struct {
	seq    int
	writes bool
	deps   []int
}

// docsPlan is the planner's answer holding items, on acme/widgets.
func docsPlan(t *testing.T, items ...docsItem) string {
	t.Helper()
	rows := make([]map[string]any, 0, len(items))
	for _, it := range items {
		deps := []map[string]any{}
		for _, d := range it.deps {
			deps = append(deps, map[string]any{"seq": d, "because": fmt.Sprintf("uses item %d's work", d)})
		}
		row := map[string]any{"seq": it.seq, "title": fmt.Sprintf("Item %d", it.seq), "brief": fmt.Sprintf("Item %d.", it.seq), "acceptance": "go test ./... passes", "depends_on": deps, "repo": "acme/widgets"}
		if it.writes {
			row["writes_shared_docs"] = true
		}
		rows = append(rows, row)
	}
	b, err := json.Marshal(map[string]any{"items": rows})
	if err != nil {
		t.Fatal(err)
	}
	return "```json\n" + string(b) + "\n```"
}

// planDocsOnce runs the planner on the seeded goal, as planFilesOnce
// does, with answer as its plan, and returns the planGoal call's items
// by seq and node.log.
func planDocsOnce(t *testing.T, answer string) (map[int]map[string]any, string) {
	t.Helper()
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{texts: []string{answer}}
	var logs bytes.Buffer
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logs, "", 0)}
	if err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0]); err != nil {
		t.Fatalf("plan: %v\n%s", err, logs.String())
	}
	if len(h.jobs) != 1 {
		t.Fatalf("planner sessions = %d, want 1", len(h.jobs))
	}
	return plannedBySeq(t, f), logs.String()
}

// (3) The item marked writes_shared_docs hands in the writer paragraph;
// every other item's brief carries the do-not-edit sentence naming it.
func TestPlanSharedDocsWriterMarked(t *testing.T) {
	items, _ := planDocsOnce(t, docsPlan(t,
		docsItem{seq: 1},
		docsItem{seq: 2},
		docsItem{seq: 3, writes: true},
	))
	writerBrief := items[3]["brief"].(string)
	if !strings.Contains(writerBrief, "Writes the goal's shared documents") {
		t.Fatalf("item 3's brief lacks the writer paragraph:\n%s", writerBrief)
	}
	if strings.Contains(writerBrief, "Do not edit the goal's shared documents") {
		t.Fatalf("item 3's brief, the writer's own, carries the do-not-edit sentence:\n%s", writerBrief)
	}
	for _, seq := range []int{1, 2} {
		brief := items[seq]["brief"].(string)
		if !strings.Contains(brief, "Do not edit the goal's shared documents (ADRs, TRD sections, AGENTS.md, README, the goal doc): item 3 writes them.") {
			t.Fatalf("item %d's brief lacks the do-not-edit sentence naming item 3:\n%s", seq, brief)
		}
		if strings.Contains(brief, "Writes the goal's shared documents") {
			t.Fatalf("item %d's brief wrongly carries the writer paragraph:\n%s", seq, brief)
		}
	}
}

// (4) The writer's brief lists every sibling by seq and title under
// "Fold in the notes left on items".
func TestPlanSharedDocsWriterListsSiblings(t *testing.T) {
	items, _ := planDocsOnce(t, docsPlan(t,
		docsItem{seq: 1},
		docsItem{seq: 2},
		docsItem{seq: 3, writes: true},
	))
	writerBrief := items[3]["brief"].(string)
	want := "Fold in the notes left on items 1 (Item 1), 2 (Item 2)"
	if !strings.Contains(writerBrief, want) {
		t.Fatalf("item 3's brief lacks %q:\n%s", want, writerBrief)
	}
}

// With no item marked, the highest-seq item nothing depends on is the
// writer.
func TestPlanSharedDocsWriterUnmarkedPicksTheHighestSeqSink(t *testing.T) {
	items, _ := planDocsOnce(t, docsPlan(t,
		docsItem{seq: 1},
		docsItem{seq: 2, deps: []int{1}},
		docsItem{seq: 3, deps: []int{1}},
	))
	writerBrief := items[3]["brief"].(string)
	if !strings.Contains(writerBrief, "Writes the goal's shared documents") {
		t.Fatalf("item 3 (the highest-seq sink) should be the writer:\n%s", writerBrief)
	}
	for _, seq := range []int{1, 2} {
		brief := items[seq]["brief"].(string)
		if !strings.Contains(brief, "item 3 writes them") {
			t.Fatalf("item %d's brief should name item 3 as the writer:\n%s", seq, brief)
		}
	}
}

// Two items marked writes_shared_docs produce a re-ask flag naming
// both, and the plan is re-asked rather than handed in with two
// writers.
func TestPlanSharedDocsWriterTwoMarkedReAsk(t *testing.T) {
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	twoMarked := docsPlan(t, docsItem{seq: 1, writes: true}, docsItem{seq: 2, writes: true})
	oneMarked := docsPlan(t, docsItem{seq: 1}, docsItem{seq: 2, writes: true})
	h := &fakeHarness{texts: []string{twoMarked, oneMarked}}
	var logs bytes.Buffer
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logs, "", 0)}
	if err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0]); err != nil {
		t.Fatalf("plan: %v\n%s", err, logs.String())
	}
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2 (a re-ask)", len(h.jobs))
	}
	if want := "items 1 and 2 both claim the shared documents; one writes them"; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks the re-ask flag %q:\n%s", want, logs.String())
	}
	items := plannedBySeq(t, f)
	writerBrief := items[2]["brief"].(string)
	if !strings.Contains(writerBrief, "Writes the goal's shared documents") {
		t.Fatalf("the final answer's item 2 should be the writer:\n%s", writerBrief)
	}
}

// sharedDocsWriter and sharedDocsWriterFlags directly.
func TestSharedDocsWriter(t *testing.T) {
	spec := func(seq int, writes bool, deps ...int) harness.PlannedSpec {
		sp := harness.PlannedSpec{Seq: seq, Title: fmt.Sprintf("Item %d", seq), WritesSharedDocs: writes}
		for _, d := range deps {
			sp.DependsOn = append(sp.DependsOn, harness.PlannedDep{Seq: d})
		}
		return sp
	}
	cases := []struct {
		name  string
		specs []harness.PlannedSpec
		want  int
	}{
		{"marked wins", []harness.PlannedSpec{spec(1, false), spec(2, true), spec(3, false, 2)}, 2},
		{"highest marked of several", []harness.PlannedSpec{spec(1, true), spec(2, true)}, 2},
		{"no mark: the highest-seq sink", []harness.PlannedSpec{spec(1, false), spec(2, false, 1), spec(3, false, 1)}, 3},
	}
	for _, c := range cases {
		if got := sharedDocsWriter(c.specs); got != c.want {
			t.Errorf("%s: sharedDocsWriter = %d, want %d", c.name, got, c.want)
		}
	}

	none := []harness.PlannedSpec{spec(1, false), spec(2, false)}
	if got := sharedDocsWriterFlags(none); got != nil {
		t.Errorf("sharedDocsWriterFlags(none marked) = %v, want nil", got)
	}
	one := []harness.PlannedSpec{spec(1, true), spec(2, false)}
	if got := sharedDocsWriterFlags(one); got != nil {
		t.Errorf("sharedDocsWriterFlags(one marked) = %v, want nil", got)
	}
	two := []harness.PlannedSpec{spec(1, true), spec(2, true)}
	if got := sharedDocsWriterFlags(two); len(got) != 1 || !strings.Contains(got[0], "items 1 and 2 both claim the shared documents; one writes them") {
		t.Errorf("sharedDocsWriterFlags(two marked) = %v", got)
	}
}
