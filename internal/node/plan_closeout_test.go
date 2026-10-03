package node

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// fleetStalls reads a fleet-stalls-loudly plan fixture as the planner's
// message.
func fleetStalls(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "plans", name))
	if err != nil {
		t.Fatal(err)
	}
	return "```json\n" + string(b) + "\n```"
}

// ADR-023's close-out rule (planCloseOutRule): the-fleet-stalls-loudly's
// item 22 — "the landed branches read as one design" — restates item
// 19's docs in prose of its own, a pull request an evaluator never
// checks against anything but a feeling. closeOutAcceptance flags it;
// the folded graph, item 19's own acceptance carrying the doc clause, is
// clean.
func TestCloseOutAcceptanceFleetStallsLoudly(t *testing.T) {
	before, err := harness.ParsePlan(fleetStalls(t, "closeout_before.json"))
	if err != nil {
		t.Fatal(err)
	}
	flags := closeOutAcceptance(before)
	if len(flags) != 1 {
		t.Fatalf("flags = %d, want 1:\n%s", len(flags), strings.Join(flags, "\n"))
	}
	want := `item 22 (The landed branches read as one design: README, ADR 017 and the TRD describe the stall report): a docs-only close-out — its acceptance only amends documents to describe landed work ("read as one design")`
	if !strings.HasPrefix(flags[0], want) {
		t.Errorf("flag = %q, want it to start %q", flags[0], want)
	}
	if !strings.Contains(flags[0], "docs land in the pull request of the change they describe") {
		t.Errorf("flag lacks the AGENTS.md rule: %q", flags[0])
	}

	after, err := harness.ParsePlan(fleetStalls(t, "closeout_after.json"))
	if err != nil {
		t.Fatal(err)
	}
	if flags := closeOutAcceptance(after); len(flags) != 0 {
		t.Fatalf("the after graph is flagged:\n%s", strings.Join(flags, "\n"))
	}
}

// A title alone can read as a close-out, even when the acceptance below
// it only names document sections.
func TestCloseOutAcceptanceTitleAlone(t *testing.T) {
	specs := []harness.PlannedSpec{{
		Seq: 1, Title: "The landed branches read as one design: the stall report",
		Acceptance: "README's Observability section and the TRD's §6b describe the stall-report shape, in the same words.",
	}}
	flags := closeOutAcceptance(specs)
	if len(flags) != 1 {
		t.Fatalf("flags = %d, want 1: %v", len(flags), flags)
	}
	if !strings.Contains(flags[0], "read as one design") {
		t.Errorf("flag = %q, want the phrase", flags[0])
	}
}

// An item whose acceptance only moves an ADR's status to accepted is
// flagged the same way (release-tag-mirrors' shape: a status line, not
// a change).
func TestCloseOutAcceptanceADRStatus(t *testing.T) {
	specs := []harness.PlannedSpec{{
		Seq: 25, Title: "ADR 018 status accepted",
		Acceptance: "the ADR's status line reads accepted.",
	}}
	flags := closeOutAcceptance(specs)
	if len(flags) != 1 {
		t.Fatalf("flags = %d, want 1: %v", len(flags), flags)
	}
	if !strings.Contains(flags[0], "docs land in the pull request of the change they describe") {
		t.Errorf("flag = %q, want the AGENTS.md rule", flags[0])
	}
}

// An item that amends an ADR's status beside its own code or test check
// is never flagged: the ADR amendment rides a real change, not a
// close-out of its own.
func TestCloseOutAcceptanceADRBesideCode(t *testing.T) {
	specs := []harness.PlannedSpec{{
		Seq: 26, Title: "ADR 018: the retry window, implemented",
		Acceptance: "`go test ./internal/node/` passes; the retry window is halved; the ADR's status line reads accepted.",
	}}
	if flags := closeOutAcceptance(specs); len(flags) != 0 {
		t.Fatalf("flagged: %v", flags)
	}
}

// A Measure: item is never a close-out: it names its own script and a
// recorded reading, not a document describing someone else's work.
func TestCloseOutAcceptanceMeasureItemNotFlagged(t *testing.T) {
	specs := []harness.PlannedSpec{{
		Seq: 9, Title: "Measure: the mini's load p90 over a day",
		Acceptance: "`bash scripts/mini/adr-029-measure.sh --dry-run` runs and prints a reading; docs/adr/029's After section records the reading it took, with when and under what load, whatever the number.",
	}}
	if flags := closeOutAcceptance(specs); len(flags) != 0 {
		t.Fatalf("flagged: %v", flags)
	}
}

// planFleetStalls runs the planner on the fleet-stalls-loudly fixtures,
// their repo the seeded one.
func planFleetStalls(t *testing.T, answers ...string) (*gatewaytest.Fake, *fakeHarness, string) {
	t.Helper()
	f := seed(t)
	f.PlanQuestions = true
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "33333333-3333-4333-8333-333333333333", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{texts: answers}
	var logs bytes.Buffer
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logs, "", 0)}
	if err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "fleet-stalls-loudly", Title: "The fleet stalls loudly", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0]); err != nil {
		t.Fatal(err)
	}
	return f, h, logs.String()
}

// A first answer that gives a close-out item an acceptance of its own is
// re-asked with the flag and planCloseOutRule; the second, folding the
// clause into the item it describes, is handed in.
func TestPlannerReasksOnACloseOut(t *testing.T) {
	f, h, _ := planFleetStalls(t, fleetStalls(t, "closeout_before.json"), fleetStalls(t, "closeout_after.json"))
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	re := h.jobs[1].Prompt
	for _, want := range []string{
		"## Your last answer was flagged",
		`item 22 (The landed branches read as one design: README, ADR 017 and the TRD describe the stall report): a docs-only close-out`,
		"docs land in the pull request of the change they describe",
		// planCloseOutRule's own text, which rides every re-ask
		"An item whose acceptance only amends documents to describe landed work",
		"this is the last ask",
	} {
		if !strings.Contains(re, want) {
			t.Fatalf("re-ask lacks %q:\n%s", want, re)
		}
	}
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	items := calls[0].Input["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["seq"] != float64(19) {
		t.Fatalf("handed in: %v", items)
	}
}
