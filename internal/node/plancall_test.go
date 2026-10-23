package node

import (
	"context"
	"fmt"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/jobresult"
	"projectmesh.io/mesh-cli/internal/mcptool"
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

// A plan handed in by tool (the scripted session's mesh_plan call as
// its mesh_result plan), with an empty final message, is the plan: its
// items go through the same checks and are handed in with their models
// and dependencies.
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
	_ = logs
}

// A scripted graph in the final message is what the fake session hands
// mesh_result; the node reads the result, never the text.
func TestPlannerPlansOnTheScriptedGraph(t *testing.T) {
	items, logs := planOn(t, "```json\n{\"items\": ["+
		"{\"seq\": 1, \"title\": \"Docs\", \"acceptance\": \"README has a Models section\", \"repo\": \"acme/widgets\", \"model\": \"haiku\", \"model_reason\": \"docs-only\"},"+
		"{\"seq\": 2, \"title\": \"Scheduler\", \"acceptance\": \"go test ./... passes\", \"repo\": \"acme/widgets\", \"model\": \"opus\", \"model_reason\": \"concurrency\", \"depends_on\": [{\"seq\": 1, \"because\": \"uses the section item 1 adds\"}]}"+
		"]}\n```", nil)
	assertTwoItems(t, items)
	_ = logs
}

// The planner's prompt is the brief's guidance first when the brief
// carries one, with none of the node's role text, the node's checks and
// the finish after the goal (ADR-041).
func TestPlannerRunsOnTheBriefsGuidance(t *testing.T) {
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.SetKindGuidance(PlanKind, "You are the planner of a goal.\n\n1. Read before you plan.\n", "")
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{result: map[string]any{"plan": map[string]any{"items": []any{
		map[string]any{"seq": float64(1), "title": "Docs", "acceptance": "README has a Models section", "repo": "acme/widgets", "model": "haiku", "model_reason": "docs-only"},
	}}}}
	var logged strings.Builder
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Model: "opus", Log: log.New(&logged, "", 0)}
	if err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0]); err != nil {
		t.Fatalf("Plan: %v\n%s", err, logged.String())
	}
	if len(f.CallsNamed("planGoal")) != 1 {
		t.Fatalf("plan calls: %+v", f.CallsNamed("planGoal"))
	}
	prompt := h.jobs[0].Prompt
	if !strings.HasPrefix(prompt, "You are the planner of a goal.\n\n1. Read before you plan.\n\n# Goal: Ship it (ship-it) for acme, project mesh") {
		t.Fatalf("the prompt does not open with the guidance:\n%.400s", prompt)
	}
	for _, gone := range []string{"You are the planner of goal", "# Rules\n", "# Check main first", "mesh_plan(", "```json"} {
		if strings.Contains(prompt, gone) {
			t.Fatalf("the prompt carries the node's role text %q", gone)
		}
	}
	for _, want := range []string{"# What the node checks before it hands the plan in", "# How this job finishes", "{plan}", "`plan.items` is the graph"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(logged.String(), noGuidanceLine) {
		t.Fatalf("a brief with guidance logged %q", noGuidanceLine)
	}
}

// A planner session that hands nothing in, or gives the plan up, is the
// plan's error naming why; nothing is handed in.
func TestPlannerWithNoResultIsAnError(t *testing.T) {
	for name, h := range map[string]*fakeHarness{
		"no call": {text: "I planned it in prose."},
		"gave up": {onRun: func(job harness.Job) {
			tool, _ := mcptool.Find(job.NodeTools, jobresult.FailTool)
			_, _ = tool.Call(context.Background(), map[string]any{"error": "the goal names no repo I can read"})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := seed(t)
			f.Goals[goalID]["status"] = "planning"
			f.Projects[projID]["repoIds"] = []string{repoID}
			gw := gateway.New(f.URL(), "rs_test")
			cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
			ws := &fakeWS{root: t.TempDir()}
			pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Model: "opus", Log: log.New(&strings.Builder{}, "", 0)}
			err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0])
			want := "no result: the session made no mesh_result call"
			if name == "gave up" {
				want = "the session gave the plan up: the goal names no repo I can read"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Plan: %v, want %q", err, want)
			}
			if len(f.CallsNamed("planGoal")) != 0 {
				t.Fatal("a plan was handed in")
			}
		})
	}
}
