package node

import (
	"context"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// The narrow mode (ADR-021's amendment 2026-10-02): a goal carrying
// replacingItemId is planned as a split of that item.

// narrowAtomicPlan is one atomic item whose acceptance differs from the
// failed item's.
const narrowAtomicPlan = "```json\n{\"items\": [{\"seq\": 3, \"title\": \"The owner migration\", \"brief\": \"One migration.\", \"acceptance\": \"go test ./internal/store/... passes\", \"atomic\": \"one migration, cannot be halved\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"

// narrowOnePlan is one non-atomic item whose acceptance differs from the
// failed item's: a re-scope, not a split.
const narrowOnePlan = "```json\n{\"items\": [{\"seq\": 3, \"title\": \"Make it pass, again\", \"brief\": \"Again, carefully.\", \"acceptance\": \"go test ./internal/node/... passes\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"

// planNarrow runs the planner on the seeded goal as a narrow re-plan of
// item 2, which failed two recorded evaluations (lineage 2, attempt 2),
// with the harness's answers in turn; prepare adjusts the fake first.
func planNarrow(t *testing.T, prepare func(f *gatewaytest.Fake), answers ...string) (*gatewaytest.Fake, *fakeHarness, *strings.Builder, error) {
	t.Helper()
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Goals[goalID]["replacingItemId"] = item2
	f.WorkItems[item2]["status"] = "failed"
	f.WorkItems[item2]["lastError"] = "evaluation failed 3 times"
	f.WorkItems[item2]["lineageRootSeq"] = 2
	f.WorkItems[item2]["lineageAttempt"] = 2
	f.WorkItems[item2]["evaluations"] = []any{
		map[string]any{"attempt": 2, "notes": "VERDICT: FAIL", "failedClauses": []any{
			map[string]any{"clause": "transfersNode applies ownsAs to the source", "satisfy": "ownsAs read from the source node"},
		}},
		map[string]any{"attempt": 1, "notes": "VERDICT: FAIL", "failedClauses": []any{
			map[string]any{"clause": "RegisterNode pins the aggregate id", "satisfy": "the id is NodeID(owner, slug)"},
		}},
	}
	f.Projects[projID]["repoIds"] = []string{repoID}
	if prepare != nil {
		prepare(f)
	}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{texts: answers}
	var logged strings.Builder
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logged, "", 0)}
	goal := gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning", ReplacingItemID: item2, NarrowReplans: 1}
	err := pl.Plan(context.Background(), goal, scopesOf(cfg)[0])
	return f, h, &logged, err
}

func planGoalTitles(t *testing.T, f *gatewaytest.Fake) []string {
	t.Helper()
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	var titles []string
	for _, it := range calls[0].Input["items"].([]any) {
		titles = append(titles, it.(map[string]any)["title"].(string))
	}
	return titles
}

func TestNarrowReplanPromptsASplit(t *testing.T) {
	_, h, logged, err := planNarrow(t, nil, splitPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("planner sessions = %d, want 1", len(h.jobs))
	}
	p := h.jobs[0].Prompt
	for _, want := range []string{
		"## Split this item\n\nThis is a narrow re-plan: your plan replaces item 2 alone, \"Make it pass\"",
		"It is lineage 2, attempt 2 of 3.",
		"Its acceptance:\n\ngo test passes\n\n",
		"The clauses its evaluations failed, oldest first:\n\n" +
			"- RegisterNode pins the aggregate id — would be satisfied by: the id is NodeID(owner, slug)\n" +
			"- transfersNode applies ownsAs to the source — would be satisfied by: ownsAs read from the source node\n",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("plan prompt lacks %q:\n%s", want, p)
		}
	}
	if want := "goal ship-it: narrow re-plan replacing item 2 (lineage 2, attempt 2 of 3): asking for a split\n"; !strings.Contains(logged.String(), want) {
		t.Fatalf("log lacks %q:\n%s", want, logged)
	}
}

func TestNarrowReplanWithOneItemIsAskedAgain(t *testing.T) {
	f, h, _, err := planNarrow(t, nil, narrowOnePlan, splitPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	want := "- narrow re-plan of item 2 (lineage 2) must replace it with two or more items whose acceptance partitions it, or one atomic item with a reason\n"
	if !strings.Contains(h.jobs[1].Prompt, want) {
		t.Fatalf("re-ask lacks %q:\n%s", want, h.jobs[1].Prompt)
	}
	if !strings.Contains(h.jobs[1].Prompt, "## Split this item") {
		t.Fatalf("re-ask lost the split section:\n%s", h.jobs[1].Prompt)
	}
	if got := planGoalTitles(t, f); len(got) != 2 || got[0] != "The owner's id" || got[1] != "The transfer" {
		t.Fatalf("handed in %q, want the two split items", got)
	}
}

func TestNarrowReplanAtomicOnce(t *testing.T) {
	// the replaced item had no atomic: line — one atomic item is handed in
	f, h, _, err := planNarrow(t, nil, narrowAtomicPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("planner sessions = %d, want 1", len(h.jobs))
	}
	if got := planGoalTitles(t, f); len(got) != 1 || got[0] != "The owner migration" {
		t.Fatalf("handed in %q, want the atomic item", got)
	}

	// it had: the lineage's exemption is spent, asked again, then refused
	wasAtomic := func(f *gatewaytest.Fake) {
		f.WorkItems[item2]["brief"] = "atomic: one migration, cannot be halved\n\nThe owner model."
	}
	f, h, _, err = planNarrow(t, wasAtomic, narrowAtomicPlan, narrowAtomicPlan)
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	if !strings.Contains(h.jobs[1].Prompt, "item 2 was already atomic (one migration, cannot be halved), so the lineage's exemption is spent") {
		t.Fatalf("re-ask does not say the exemption is spent:\n%s", h.jobs[1].Prompt)
	}
	if err == nil || !strings.Contains(err.Error(), "lineage 2") {
		t.Fatalf("err = %v, want a refusal naming lineage 2", err)
	}
	if calls := f.CallsNamed("planGoal"); len(calls) != 0 {
		t.Fatalf("a spent atomic was handed in: %+v", calls)
	}
}

func TestNarrowReplanSecondAnswerStillOneItemIsRefused(t *testing.T) {
	f, h, _, err := planNarrow(t, nil, narrowOnePlan, narrowOnePlan)
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	if err == nil || err.Error() != "narrow re-plan of item 2 (lineage 2) answered 1 item(s) and no split; refused" {
		t.Fatalf("err = %v", err)
	}
	if calls := f.CallsNamed("planGoal"); len(calls) != 0 {
		t.Fatalf("an unsplit narrow plan was handed in: %+v", calls)
	}
}

func TestWholePlanUnchangedWithoutReplacingItemId(t *testing.T) {
	// a whole re-plan of the same goal: one item is fine, no split asked
	f, h, logged, err := planReplan(t, narrowOnePlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("planner sessions = %d, want 1", len(h.jobs))
	}
	if strings.Contains(h.jobs[0].Prompt, "## Split this item") {
		t.Fatalf("a whole plan has the split section:\n%s", h.jobs[0].Prompt)
	}
	if strings.Contains(logged.String(), "narrow re-plan") {
		t.Fatalf("a whole plan logged a narrow re-plan:\n%s", logged)
	}
	if got := planGoalTitles(t, f); len(got) != 1 {
		t.Fatalf("handed in %q", got)
	}
	TestPlannerReplans(t)
}
