package node

import (
	"context"
	"fmt"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// ADR-023 (a), the size rule: a graph over the project's maxItems is not
// handed in; the re-ask asks for `split`, and the drafts go to
// ProposeGoalSplit.

// cappedPlan is a graph of n items on acme/widgets, each its own check.
func cappedPlan(n int) string {
	items := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		items = append(items, fmt.Sprintf(`{"seq": %d, "title": "Part %d", "brief": "Part %d.", "acceptance": "go test ./internal/part%d/... passes", "depends_on": [], "repo": "acme/widgets"}`, i, i, i, i))
	}
	return "```json\n{\"items\": [" + strings.Join(items, ",") + "]}\n```"
}

// splitAnswer is the planner's `split`: two drafts, 5 and 7 items by
// their statements, one repo each.
const splitAnswer = "```json\n{\"split\": [" +
	`{"slug": "ship-it-core", "title": "Ship it: the core", "statement": "Five items: parts 1-5.", "acceptance": "go test ./internal/core/... passes", "repo": "acme/widgets"},` +
	`{"slug": "ship-it-edges", "title": "Ship it: the edges", "statement": "Seven items: parts 6-12.", "acceptance": "go test ./internal/edges/... passes", "repo": "widgets"}` +
	"]}\n```"

// planCapped runs the planner on the seeded goal with the harness's
// answers in turn; maxItems > 0 seeds the project's own cap and split
// gives the fake proposeGoalSplit.
func planCapped(t *testing.T, maxItems int, split bool, answers ...string) (*gatewaytest.Fake, *fakeHarness, *strings.Builder, error) {
	t.Helper()
	f := seed(t)
	f.GoalSplit = split
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	if maxItems > 0 {
		f.Projects[projID]["maxItems"] = maxItems
	}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{texts: answers}
	var logged strings.Builder
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logged, "", 0)}
	err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0])
	return f, h, &logged, err
}

const capFlag = "- the plan has 12 items, over the project's cap of 8: do not send items — answer `split`: goal drafts (slug, title, statement, acceptance, repo), each at most 8 items and one repo"

func TestOverCap(t *testing.T) {
	specs := make([]harness.PlannedSpec, 12)
	if got, want := overCap(specs, 8), strings.TrimPrefix(capFlag, "- "); got != want {
		t.Fatalf("overCap = %q, want %q", got, want)
	}
	if got := overCap(specs[:8], 8); got != "" {
		t.Fatalf("8 items flagged: %q", got)
	}
	if got := overCap(specs[:6], 5); !strings.Contains(got, "6 items, over the project's cap of 5") {
		t.Fatalf("a cap of 5: %q", got)
	}
}

// 12 items on a project with maxItems 8: asked again for `split`, and
// the drafts go to proposeGoalSplit in one call; planGoal is never
// called.
func TestPlannerOverCapProposesSplit(t *testing.T) {
	f, h, logged, err := planCapped(t, 8, true, cappedPlan(12), splitAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	if !strings.Contains(h.jobs[0].Prompt, "Two to 8 items.") {
		t.Fatalf("first prompt does not name the cap:\n%s", h.jobs[0].Prompt)
	}
	for _, want := range []string{capFlag, "answer `split`, not `items` — goal drafts partitioning the goal, each at most 8 items and one repo"} {
		if !strings.Contains(h.jobs[1].Prompt, want) {
			t.Fatalf("re-ask lacks %q:\n%s", want, h.jobs[1].Prompt)
		}
	}
	if calls := f.CallsNamed("planGoal"); len(calls) != 0 {
		t.Fatalf("a plan over the cap was handed in: %+v", calls)
	}
	calls := f.CallsNamed("proposeGoalSplit")
	if len(calls) != 1 {
		t.Fatalf("proposeGoalSplit calls = %+v, want one", calls)
	}
	drafts := calls[0].Input["drafts"].([]any)
	if len(drafts) != 2 {
		t.Fatalf("drafts = %v, want two", drafts)
	}
	for i, want := range []map[string]any{
		{"slug": "ship-it-core", "title": "Ship it: the core", "statement": "Five items: parts 1-5.", "acceptance": "go test ./internal/core/... passes", "repoId": repoID},
		{"slug": "ship-it-edges", "title": "Ship it: the edges", "statement": "Seven items: parts 6-12.", "acceptance": "go test ./internal/edges/... passes", "repoId": repoID},
	} {
		got := drafts[i].(map[string]any)
		for k, v := range want {
			if got[k] != v {
				t.Errorf("draft %d %s = %v, want %v", i, k, got[k], v)
			}
		}
	}
	if !strings.Contains(logged.String(), "goal ship-it: over the cap (12 items > 8); proposed a split into 2 goals: ship-it-core, ship-it-edges") {
		t.Fatalf("the split is not logged:\n%s", logged)
	}
}

// A project serving no maxItems — every platform today — caps at 8.
func TestPlannerCapDefaultsToEight(t *testing.T) {
	f, h, _, err := planCapped(t, 0, true, cappedPlan(12), splitAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 2 || !strings.Contains(h.jobs[1].Prompt, capFlag) {
		t.Fatalf("no served maxItems is not a cap of 8: %d sessions", len(h.jobs))
	}
	if len(f.CallsNamed("proposeGoalSplit")) != 1 || len(f.CallsNamed("planGoal")) != 0 {
		t.Fatal("the split was not proposed")
	}
}

// A second answer still over the cap is refused, naming the cap, and
// nothing is handed in.
func TestPlannerCapRefusesASecondOverCap(t *testing.T) {
	f, h, _, err := planCapped(t, 8, true, cappedPlan(12), cappedPlan(12))
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	if err == nil || err.Error() != "plan has 12 items, over the project's cap of 8, and was not answered `split`; refused" {
		t.Fatalf("err = %v", err)
	}
	if len(f.CallsNamed("planGoal")) != 0 || len(f.CallsNamed("proposeGoalSplit")) != 0 {
		t.Fatal("a refused plan handed something in")
	}
}

// A split draft naming two repos, or none of the project's, is refused
// naming the cap.
func TestPlannerSplitRefusesAForeignRepo(t *testing.T) {
	bad := strings.Replace(splitAnswer, `"repo": "widgets"`, `"repo": "acme/widgets, acme/gadgets"`, 1)
	f, _, _, err := planCapped(t, 8, true, cappedPlan(12), bad)
	if err == nil || !strings.Contains(err.Error(), `split draft ship-it-edges names repo "acme/widgets, acme/gadgets", not one of the project's; each draft is at most 8 items and one repo`) {
		t.Fatalf("err = %v", err)
	}
	if len(f.CallsNamed("planGoal")) != 0 || len(f.CallsNamed("proposeGoalSplit")) != 0 {
		t.Fatal("a refused split handed something in")
	}
}

// A platform without proposeGoalSplit: each draft is logged, the error
// leaves the goal in planning, and planGoal is not called.
func TestPlannerSplitWithoutGoalSplit(t *testing.T) {
	f, _, logged, err := planCapped(t, 8, false, cappedPlan(12), splitAnswer)
	if err == nil || err.Error() != "plan has 12 items, over the cap of 8; split into 2 goals proposed, which this platform cannot take" {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"plan ship-it: proposed goal ship-it-core: Ship it: the core", "plan ship-it: proposed goal ship-it-edges: Ship it: the edges"} {
		if !strings.Contains(logged.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, logged)
		}
	}
	if len(f.CallsNamed("planGoal")) != 0 || len(f.CallsNamed("proposeGoalSplit")) != 0 {
		t.Fatal("something was handed in")
	}
}

// 8 items fit: handed in with planGoal as before, asked once.
func TestPlannerAtCapHandsIn(t *testing.T) {
	f, h, _, err := planCapped(t, 8, true, cappedPlan(8))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("planner sessions = %d, want 1", len(h.jobs))
	}
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 || len(calls[0].Input["items"].([]any)) != 8 {
		t.Fatalf("planGoal calls = %+v", calls)
	}
	if len(f.CallsNamed("proposeGoalSplit")) != 0 {
		t.Fatal("a plan at the cap was split")
	}
}
