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

// cifits reads a ci-fits-the-mini plan fixture as the planner's message.
func cifits(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "plans", name))
	if err != nil {
		t.Fatal(err)
	}
	return "```json\n" + string(b) + "\n```"
}

// ADR-020: ci-fits-the-mini's graph as it was planned gates item 5 on
// its shards' durations and an item on the mini's load p90, readings of
// the host no pull request can change; the graph as item 28 had it
// proves the balance by planned seconds, measures the load in a
// `Measure:` item and asks the person about the p90 clause.
func TestHostReadingAcceptanceCIFitsTheMini(t *testing.T) {
	before, err := harness.ParsePlan(cifits(t, "cifits_before.json"))
	if err != nil {
		t.Fatal(err)
	}
	flags := hostReadingAcceptance(before)
	if len(flags) != 2 {
		t.Fatalf("flags = %d, want 2:\n%s", len(flags), strings.Join(flags, "\n"))
	}
	for i, want := range []string{
		`item 2 (Mini load: the self-hosted runners keep the host's load down): its acceptance names a reading of the host as a merge gate ("load p90 under 1.5 × cores")`,
		`item 5 (Shard the e2e suite in three): its acceptance names a reading of the host as a merge gate ("durations within 20%")`,
	} {
		if !strings.HasPrefix(flags[i], want) {
			t.Errorf("flag %d = %q, want it to start %q", i, flags[i], want)
		}
	}

	after, err := harness.ParsePlan(cifits(t, "cifits_after.json"))
	if err != nil {
		t.Fatal(err)
	}
	if flags := hostReadingAcceptance(after); len(flags) != 0 {
		t.Fatalf("the after graph is flagged:\n%s", strings.Join(flags, "\n"))
	}
	var measure *harness.PlannedSpec
	for i := range after {
		if strings.HasPrefix(after[i].Title, "Measure: ") {
			measure = &after[i]
		}
	}
	if measure == nil || measure.Title != "Measure: the mini's load p90 over a day" ||
		!strings.Contains(measure.Acceptance, "scripts/mini/adr-029-measure.sh") || !strings.Contains(measure.Acceptance, "records the reading it took") {
		t.Fatalf("the Measure item: %+v", measure)
	}
	// the Measure item is exempt by its title alone: the same acceptance
	// on another title is flagged
	if len(hostReadingAcceptance([]harness.PlannedSpec{{Seq: 9, Title: "Load", Acceptance: "the mini's load p90 under 1.5 × cores"}})) != 1 {
		t.Error("a load threshold outside a Measure item is not flagged")
	}

	qs := harness.ParsePlanQuestions(cifits(t, "cifits_after.json"))
	if len(qs) != 1 || qs[0].Clause != "the mini's load p90 under 1.5 × cores" || !strings.Contains(qs[0].Question, "no pull request can satisfy") {
		t.Fatalf("questions = %+v", qs)
	}
	if qs := harness.ParsePlanQuestions(cifits(t, "cifits_before.json")); qs != nil {
		t.Fatalf("the before graph asks %+v", qs)
	}
}

// A reading with no threshold, or a threshold on no reading, is not one.
func TestHostReadingAcceptanceNeedsBoth(t *testing.T) {
	for _, acc := range []string{
		"`go test ./...` passes; the balance is proven by planned-seconds balance and disjoint shard lists.",
		"the README records each shard's durations from a sequential run.",
		"the diff is under 200 lines; `go vet` passes.",
		"loaded config is under test.",
	} {
		if flags := hostReadingAcceptance([]harness.PlannedSpec{{Seq: 1, Title: "T", Acceptance: acc}}); len(flags) != 0 {
			t.Errorf("%q flagged: %v", acc, flags)
		}
	}
	for _, acc := range []string{
		"e2e wall time at most 12 minutes.",
		"each shard takes < 300 seconds.",
		"the load stays below 27.",
	} {
		if flags := hostReadingAcceptance([]harness.PlannedSpec{{Seq: 1, Title: "T", Acceptance: acc}}); len(flags) != 1 {
			t.Errorf("%q not flagged", acc)
		}
	}
}

// planCIFits runs the planner on the ci-fits-the-mini fixtures, their
// repo the seeded one, on a platform whose PlanGoalInput has questions
// or not; it returns node.log too.
func planCIFits(t *testing.T, takesQuestions bool, answers ...string) (*gatewaytest.Fake, *fakeHarness, string) {
	t.Helper()
	f := seed(t)
	f.PlanQuestions = takesQuestions
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	for i, a := range answers {
		answers[i] = strings.ReplaceAll(a, "runsheet/runsheet", "acme/widgets")
	}
	h := &fakeHarness{texts: answers}
	var logs bytes.Buffer
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logs, "", 0)}
	if err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ci-fits-the-mini", Title: "CI fits the mini", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0]); err != nil {
		t.Fatal(err)
	}
	return f, h, logs.String()
}

// A first answer that gates items on host readings is re-asked with the
// flags and the rule in the prompt; the second, clean, is handed in.
func TestPlannerReasksOnAHostReading(t *testing.T) {
	f, h, _ := planCIFits(t, true, cifits(t, "cifits_before.json"), cifits(t, "cifits_after.json"))
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	re := h.jobs[1].Prompt
	for _, want := range []string{
		"## Your last answer was flagged",
		`- item 2 (Mini load: the self-hosted runners keep the host's load down): its acceptance names a reading of the host as a merge gate ("load p90 under 1.5 × cores")`,
		`- item 5 (Shard the e2e suite in three): its acceptance names a reading of the host as a merge gate ("durations within 20%")`,
		"An item's acceptance names only what a pull request can change",
		"its own item titled `Measure: …`",
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
	if len(items) != 3 || items[1].(map[string]any)["title"] != "Measure: the mini's load p90 over a day" {
		t.Fatalf("handed in: %v", items)
	}
}

// A platform whose PlanGoalInput has questions is sent them with the
// plan; one without gets the plan alone, and each question is logged
// for the person.
func TestPlannerHandsInQuestionsOnlyWhereTaken(t *testing.T) {
	f, _, logs := planCIFits(t, true, cifits(t, "cifits_after.json"))
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	qs, _ := calls[0].Input[gateway.PlanQuestionsField].([]any)
	if len(qs) != 1 || qs[0].(map[string]any)["clause"] != "the mini's load p90 under 1.5 × cores" {
		t.Fatalf("questions sent = %#v", calls[0].Input[gateway.PlanQuestionsField])
	}
	if strings.Contains(logs, "question for the person:") {
		t.Errorf("a question the platform took was logged too:\n%s", logs)
	}

	f, _, logs = planCIFits(t, false, cifits(t, "cifits_after.json"))
	calls = f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	if _, sent := calls[0].Input[gateway.PlanQuestionsField]; sent {
		t.Fatalf("questions sent to a platform without the field: %#v", calls[0].Input)
	}
	if want := "plan ci-fits-the-mini: question for the person: the mini's load p90 under 1.5 × cores — The load p90 is a reading of the host"; strings.Count(logs, "question for the person:") != 1 || !strings.Contains(logs, want) {
		t.Fatalf("log lacks %q once:\n%s", want, logs)
	}
	if !strings.Contains(logs, "planned, 3 items and a question for the person") {
		t.Errorf("the planned line does not count the question:\n%s", logs)
	}
}
