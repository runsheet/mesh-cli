package node

import (
	"context"
	"fmt"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// planOn plans goal ship-it on a session ending with says and calls and
// returns the fake's planGoal items and the node's log (ADR-023).
func planOn(t *testing.T, says string, calls map[string]map[string]any) ([]any, string) {
	t.Helper()
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{texts: []string{says}, workCalls: calls}
	var logged strings.Builder
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Model: "opus", Log: log.New(&logged, "", 0)}
	if err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0]); err != nil {
		t.Fatalf("Plan: %v\n%s", err, logged.String())
	}
	if len(h.jobs) != 1 {
		t.Fatalf("%d planner sessions, want 1", len(h.jobs))
	}
	calls2 := f.CallsNamed("planGoal")
	if len(calls2) != 1 {
		t.Fatalf("plan calls: %+v", calls2)
	}
	items, _ := calls2[0].Input["items"].([]any)
	return items, logged.String()
}

// assertTwoItems checks the two planned items' models and item 2's
// dependency on item 1.
func assertTwoItems(t *testing.T, items []any) {
	t.Helper()
	if len(items) != 2 {
		t.Fatalf("planGoal items %v, want two", items)
	}
	one, two := items[0].(map[string]any), items[1].(map[string]any)
	if one["model"] != "haiku" || two["model"] != "opus" {
		t.Errorf("models %v, %v", one["model"], two["model"])
	}
	if deps := fmt.Sprint(two["dependsOn"]); deps != "[1]" {
		t.Errorf("item 2 dependsOn %s, want [1]: %v", deps, two)
	}
	if deps := fmt.Sprint(one["dependsOn"]); deps != "[]" && deps != "<nil>" {
		t.Errorf("item 1 dependsOn %s", deps)
	}
}

// A mesh_plan call with an empty final message is the plan: its items
// go through the same checks and are handed in with their models and
// dependencies.
func TestPlannerPlansOnTheMeshPlanCall(t *testing.T) {
	items, logs := planOn(t, "", map[string]map[string]any{"mesh_plan": {
		"goal_id": goalID,
		"items": []any{
			map[string]any{"seq": float64(1), "title": "Docs", "acceptance": "README has a Models section", "repo": "acme/widgets", "model": "haiku", "model_reason": "docs-only"},
			map[string]any{"seq": float64(2), "title": "Scheduler", "acceptance": "go test ./... passes", "repo": "acme/widgets", "model": "opus", "model_reason": "concurrency",
				"depends_on": []any{map[string]any{"seq": float64(1), "because": "uses the section item 1 adds"}}},
		},
		"questions": []any{},
	}})
	assertTwoItems(t, items)
	if strings.Contains(logs, noPlanCallLine) {
		t.Fatalf("a tool-call plan logged the fallback:\n%s", logs)
	}
}

// With no call the fenced JSON is parsed as before, logged once.
func TestPlannerTextIsTheLoggedFallback(t *testing.T) {
	items, logs := planOn(t, "```json\n{\"items\": ["+
		"{\"seq\": 1, \"title\": \"Docs\", \"acceptance\": \"README has a Models section\", \"repo\": \"acme/widgets\", \"model\": \"haiku\", \"model_reason\": \"docs-only\"},"+
		"{\"seq\": 2, \"title\": \"Scheduler\", \"acceptance\": \"go test ./... passes\", \"repo\": \"acme/widgets\", \"model\": \"opus\", \"model_reason\": \"concurrency\", \"depends_on\": [{\"seq\": 1, \"because\": \"uses the section item 1 adds\"}]}"+
		"]}\n```", nil)
	assertTwoItems(t, items)
	if n := strings.Count(logs, "plan ship-it: no mesh_plan call; parsed the final message"); n != 1 {
		t.Fatalf("fallback logged %d times:\n%s", n, logs)
	}
}
