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
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Goal items-that-share-a-file-are-planned-in-a-line: the plan check
// chains sibling items that name the same code file, the later on the
// earlier, and the hand-in's summary counts what it added.

// filesItem is one fixture item: its seq, the files it names (nil
// leaves `files` out), its brief and its dependencies (each with a
// because).
type filesItem struct {
	seq   int
	files []string
	brief string
	deps  []int
}

// filesPlan is the planner's answer holding items, on acme/widgets.
func filesPlan(t *testing.T, items ...filesItem) string {
	t.Helper()
	rows := make([]map[string]any, 0, len(items))
	for _, it := range items {
		deps := []map[string]any{}
		for _, d := range it.deps {
			deps = append(deps, map[string]any{"seq": d, "because": fmt.Sprintf("uses item %d's work", d)})
		}
		brief := it.brief
		if brief == "" {
			brief = fmt.Sprintf("Item %d.", it.seq)
		}
		row := map[string]any{"seq": it.seq, "title": fmt.Sprintf("Item %d", it.seq), "brief": brief, "acceptance": "go test ./... passes", "depends_on": deps, "repo": "acme/widgets"}
		if it.files != nil {
			row["files"] = it.files
		}
		rows = append(rows, row)
	}
	b, err := json.Marshal(map[string]any{"items": rows})
	if err != nil {
		t.Fatal(err)
	}
	return "```json\n" + string(b) + "\n```"
}

// planFilesOnce runs the planner on the seeded goal, as planGoalRepoOnce
// does, with answer as its plan, and returns the planGoal call's items
// by seq and node.log.
func planFilesOnce(t *testing.T, answer string) (map[int]map[string]any, string) {
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

// plannedBySeq is the one planGoal call's items, by seq.
func plannedBySeq(t *testing.T, f *gatewaytest.Fake) map[int]map[string]any {
	t.Helper()
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	out := map[int]map[string]any{}
	for _, it := range calls[0].Input["items"].([]any) {
		row := it.(map[string]any)
		out[int(row["seq"].(float64))] = row
	}
	return out
}

// dependsOn is a handed-in item's dependsOn as ints.
func dependsOn(row map[string]any) []int {
	var out []int
	for _, d := range row["dependsOn"].([]any) {
		out = append(out, int(d.(float64)))
	}
	return out
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// (1) Two items naming the same code file with no dependency: the later
// is handed in depending on the earlier, the plan check's line in its
// brief.
func TestPlanCheckSharedCodeFileChainsTheLater(t *testing.T) {
	items, logs := planFilesOnce(t, filesPlan(t,
		filesItem{seq: 1, files: []string{"internal/node/validation.go"}},
		filesItem{seq: 2, files: []string{"./internal/node/validation.go", "internal/node/runner.go"}},
	))
	if got := dependsOn(items[1]); len(got) != 0 {
		t.Errorf("item 1 dependsOn = %v, want none", got)
	}
	if got := dependsOn(items[2]); !intsEqual(got, []int{1}) {
		t.Fatalf("item 2 dependsOn = %v, want [1]", got)
	}
	want := "Depends on #1 because the plan check found both name internal/node/validation.go; it starts from item 1's merged work."
	if brief := items[2]["brief"].(string); !strings.Contains(brief, want) {
		t.Fatalf("item 2's brief lacks %q:\n%s", want, brief)
	}
	if want := "planned, 2 items, 1 dependencies added by the plan check (2→1 internal/node/validation.go)"; !strings.Contains(logs, want) {
		t.Errorf("log lacks %q:\n%s", want, logs)
	}
}

// (2) Two items naming different files stay independent, and the
// summary says the check added none.
func TestPlanCheckDifferentFilesStayIndependent(t *testing.T) {
	items, logs := planFilesOnce(t, filesPlan(t,
		filesItem{seq: 1, files: []string{"internal/a.go"}},
		filesItem{seq: 2, files: []string{"internal/b.go"}},
	))
	for seq := 1; seq <= 2; seq++ {
		if got := dependsOn(items[seq]); len(got) != 0 {
			t.Errorf("item %d dependsOn = %v, want none", seq, got)
		}
		if strings.Contains(items[seq]["brief"].(string), "Depends on") {
			t.Errorf("item %d's brief names a dependency:\n%s", seq, items[seq]["brief"])
		}
	}
	if want := "planned, 2 items, none added by the plan check"; !strings.Contains(logs, want) {
		t.Errorf("log lacks %q:\n%s", want, logs)
	}
}

// (2) on briefs: items with no files whose briefs share the repo's
// owner/name, a URL and a `./pkg/...` pattern but name different code
// files stay independent — none of those is a file.
func TestPlanCheckDifferentFilesInBriefsStayIndependent(t *testing.T) {
	items, logs := planFilesOnce(t, filesPlan(t,
		filesItem{seq: 1, brief: "In runsheet/mesh-cli, change `internal/a.go` against api.runsheet.dev/graphql; `go test ./internal/node/...` passes."},
		filesItem{seq: 2, brief: "In runsheet/mesh-cli, change `internal/b.go` against api.runsheet.dev/graphql; `go test ./internal/node/...` passes."},
	))
	for seq := 1; seq <= 2; seq++ {
		if got := dependsOn(items[seq]); len(got) != 0 {
			t.Errorf("item %d dependsOn = %v, want none", seq, got)
		}
	}
	if want := "planned, 2 items, none added by the plan check"; !strings.Contains(logs, want) {
		t.Errorf("log lacks %q:\n%s", want, logs)
	}
}

// namesCodeFile: a file path is a code file; a repo, a host, a package
// pattern, a directory and a document are not.
func TestPlanCheckNamesCodeFile(t *testing.T) {
	for p, want := range map[string]bool{
		"internal/node/plan_check.go":      true,
		"platform/loomgen/registry_gen.go": true,
		".github/workflows/ci.yml":         true,
		"main.go":                          true,
		"runsheet/mesh-cli":                false,
		"api.runsheet.dev/graphql":         false,
		"github.com/acme/widgets/x.go":     false,
		"internal/node/...":                false,
		"internal/node":                    false,
		"AGENTS.md":                        false,
		"docs/adr/adr-023.go":              false,
		"":                                 false,
	} {
		if got := namesCodeFile(p); got != want {
			t.Errorf("namesCodeFile(%q) = %v, want %v", p, got, want)
		}
	}
}

// (6) Negative: two items sharing only documents — AGENTS.md and an
// ADR — are not chained; the shared documents are one writer's.
func TestPlanCheckSharedDocumentsAreNotChained(t *testing.T) {
	items, logs := planFilesOnce(t, filesPlan(t,
		filesItem{seq: 1, files: []string{"AGENTS.md", "docs/adr/adr-023.md", "internal/a.go"}},
		filesItem{seq: 2, files: []string{"AGENTS.md", "docs/adr/adr-023.md", "internal/b.go"}},
	))
	if got := dependsOn(items[2]); len(got) != 0 {
		t.Fatalf("item 2 dependsOn = %v, want none: documents are not chained", got)
	}
	if !strings.Contains(logs, "none added by the plan check") {
		t.Errorf("log lacks the none line:\n%s", logs)
	}
}

// Three items sharing one file chain 1←2←3, not 1←2 and 1←3, and make
// no cycle.
func TestPlanCheckSharedFileChainsThreeInALine(t *testing.T) {
	items, logs := planFilesOnce(t, filesPlan(t,
		filesItem{seq: 1, files: []string{"internal/x.go"}},
		filesItem{seq: 2, files: []string{"internal/x.go"}},
		filesItem{seq: 3, files: []string{"internal/x.go"}},
	))
	for seq, want := range map[int][]int{1: nil, 2: {1}, 3: {2}} {
		if got := dependsOn(items[seq]); !intsEqual(got, want) {
			t.Errorf("item %d dependsOn = %v, want %v", seq, got, want)
		}
	}
	if want := "2 dependencies added by the plan check (2→1 internal/x.go; 3→2 internal/x.go)"; !strings.Contains(logs, want) {
		t.Errorf("log lacks %q:\n%s", want, logs)
	}
}

// An item with no `files` is read from the paths its brief names.
func TestPlanCheckFilesFromTheBrief(t *testing.T) {
	items, _ := planFilesOnce(t, filesPlan(t,
		filesItem{seq: 1, brief: "Add the check to internal/node/plan_check.go."},
		filesItem{seq: 2, brief: "Log it from internal/node/plan.go and internal/node/plan_check.go."},
	))
	if got := dependsOn(items[2]); !intsEqual(got, []int{1}) {
		t.Fatalf("item 2 dependsOn = %v, want [1] from the briefs' paths", got)
	}
	if !strings.Contains(items[2]["brief"].(string), "the plan check found both name internal/node/plan_check.go") {
		t.Errorf("item 2's brief lacks the plan check's line:\n%s", items[2]["brief"])
	}
}

// sharedCodeFiles itself: a dependency the planner already gave, in
// either direction and through another item, adds nothing; one per
// pair, naming the first shared file.
func TestSharedCodeFiles(t *testing.T) {
	spec := func(seq int, files []string, deps ...int) harness.PlannedSpec {
		sp := harness.PlannedSpec{Seq: seq, Files: files}
		for _, d := range deps {
			sp.DependsOn = append(sp.DependsOn, harness.PlannedDep{Seq: d})
		}
		return sp
	}
	cases := []struct {
		name  string
		specs []harness.PlannedSpec
		want  []addedDep
	}{
		{"already depends", []harness.PlannedSpec{spec(1, []string{"a.go"}), spec(2, []string{"a.go"}, 1)}, nil},
		{"the earlier depends on the later", []harness.PlannedSpec{spec(1, []string{"a.go"}, 2), spec(2, []string{"a.go"})}, nil},
		{"transitively", []harness.PlannedSpec{spec(1, []string{"a.go"}), spec(2, []string{"c.go"}, 1), spec(3, []string{"a.go"}, 2)}, nil},
		{"one per pair, the first file", []harness.PlannedSpec{spec(1, []string{"b.go", "a.go"}), spec(2, []string{"a.go", "b.go"})}, []addedDep{{Seq: 2, On: 1, File: "a.go"}}},
		{"case-sensitive", []harness.PlannedSpec{spec(1, []string{"A.go"}), spec(2, []string{"a.go"})}, nil},
		{"different repos", []harness.PlannedSpec{{Seq: 1, Repo: "acme/a", Files: []string{"x.go"}}, {Seq: 2, Repo: "acme/b", Files: []string{"x.go"}}}, nil},
		{"a sentence's full stop", []harness.PlannedSpec{{Seq: 1, Brief: "Change internal/x.go."}, {Seq: 2, Brief: "Then internal/x.go, too."}}, []addedDep{{Seq: 2, On: 1, File: "internal/x.go"}}},
		{"out of order", []harness.PlannedSpec{spec(2, []string{"a.go"}), spec(1, []string{"a.go"})}, []addedDep{{Seq: 2, On: 1, File: "a.go"}}},
	}
	for _, c := range cases {
		got := sharedCodeFiles(c.specs)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: sharedCodeFiles = %v, want %v", c.name, got, c.want)
		}
	}
}
