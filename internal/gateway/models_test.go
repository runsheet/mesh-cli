package gateway_test

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const itemProject = "cccccccc-3333-4333-8333-cccccccccccc"

// modelItem is a ready item as a platform with items-carry-a-model
// serves it: its tier, its agent, and what evaluates it.
func modelItem(f *gatewaytest.Fake, id string, seq int) {
	f.WorkItems[id] = map[string]any{
		"id": id, "orgId": orgID, "projectId": itemProject, "seq": seq, "status": "ready",
		"model": "haiku", "agentSlug": "fast-builder", "evaluateModel": "sonnet",
	}
}

// An item read from a platform that has the fields carries its model
// and its agent, whichever read it came through.
func TestWorkItemCarriesModelAndAgent(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	modelItem(f, "item-1", 1)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := nodeCtx()

	it, err := gw.WorkItem(ctx, "item-1")
	if err != nil {
		t.Fatal(err)
	}
	ready, err := gw.ReadyWorkItems(ctx, orgID)
	if err != nil || len(ready) != 1 {
		t.Fatalf("ready = %+v, %v", ready, err)
	}
	for _, got := range []gateway.WorkItem{*it, ready[0]} {
		if got.Model != "haiku" || got.AgentSlug != "fast-builder" || got.EvaluateModel != "sonnet" || got.ProjectID != itemProject {
			t.Fatalf("decoded as %+v", got)
		}
	}
}

// A platform older than the fields refuses the selection: the reads fall
// back to the base fields, return the items with no model, and stop
// asking for the fields the gateway does not know.
func TestWorkItemReadsWithoutModelOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields["model"] = true
	modelItem(f, "item-1", 1)
	gw := gateway.New(f.URL(), "rs_test")
	ctx := nodeCtx()

	for i := 0; i < 2; i++ {
		items, err := gw.ReadyWorkItems(ctx, orgID)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if len(items) != 1 || items[0].ID != "item-1" || items[0].Model != "" {
			t.Fatalf("read %d: %+v", i, items)
		}
	}
	// the first read tried the fields once; every read after asks without
	if n := len(f.ReadsNamed("workItemIndexs")); n != 3 {
		t.Fatalf("workItemIndexs read %d times, want 3", n)
	}
	it, err := gw.WorkItem(ctx, "item-1")
	if err != nil || it.Model != "" {
		t.Fatalf("WorkItem = %+v, %v", it, err)
	}
	if _, err := gw.GoalWorkItems(ctx, "goal-1"); err != nil {
		t.Fatal(err)
	}
}

// An agent's model is read by its id; a missing agent, a retired one and
// a platform without agents are all "" and no error.
func TestAgentModel(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	org := uuid.MustParse(orgID)
	id := gateway.AgentID(org, uuid.Nil, "fast-builder").String()
	retired := gateway.AgentID(uuid.Nil, uuid.Nil, "old").String()
	f.Agents[id] = map[string]any{"id": id, "slug": "fast-builder", "model": "haiku", "status": "active"}
	f.Agents[retired] = map[string]any{"id": retired, "slug": "old", "model": "opus", "status": "retired"}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := nodeCtx()

	if m, err := gw.AgentModel(ctx, id); err != nil || m != "haiku" {
		t.Fatalf("AgentModel = %q, %v", m, err)
	}
	for _, missing := range []string{gateway.AgentID(org, uuid.Nil, "nobody").String(), retired} {
		if m, err := gw.AgentModel(ctx, missing); err != nil || m != "" {
			t.Fatalf("AgentModel(%s) = %q, %v", missing, m, err)
		}
	}
	f.UnknownFields["model"] = true
	if m, err := gw.AgentModel(ctx, id); err != nil || m != "" {
		t.Fatalf("AgentModel on a platform without the field = %q, %v", m, err)
	}
}

// An item's agent resolves project, then org, then the platform's
// default; an item that names none has no ids to try.
func TestWorkItemAgentIDs(t *testing.T) {
	org, project := uuid.MustParse(orgID), uuid.MustParse(itemProject)
	it := gateway.WorkItem{OrgID: orgID, ProjectID: itemProject, AgentSlug: "fast-builder"}
	want := []string{
		gateway.AgentID(org, project, "fast-builder").String(),
		gateway.AgentID(org, uuid.Nil, "fast-builder").String(),
		gateway.AgentID(uuid.Nil, uuid.Nil, "fast-builder").String(),
	}
	if got := it.AgentIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentIDs = %v, want %v", got, want)
	}
	if got := (gateway.WorkItem{OrgID: orgID}).AgentIDs(); got != nil {
		t.Fatalf("no agent: %v", got)
	}
}

// PlanGoal sends a model for the item that has one and no key for the
// item without.
func TestPlanGoalSendsModel(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Goals["goal-1"] = map[string]any{"id": "goal-1", "status": "planning"}
	gw := gateway.New(f.URL(), "rs_test")

	err := gw.PlanGoal(nodeCtx(), "goal-1", "", []gateway.PlannedItem{
		{Seq: 1, Title: "docs", Model: "haiku"},
		{Seq: 2, Title: "the rest"},
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := planned(t, f)
	if rows[0]["model"] != "haiku" {
		t.Fatalf("item 1 sent %v", rows[0])
	}
	if _, ok := rows[1]["model"]; ok {
		t.Fatalf("item 2 sent a model: %v", rows[1])
	}
}

// A platform whose graph input has no model refuses it; PlanGoal hands
// the graph in again without, and it is accepted.
func TestPlanGoalWithoutModelOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields["model"] = true
	f.Goals["goal-1"] = map[string]any{"id": "goal-1", "status": "planning"}
	gw := gateway.New(f.URL(), "rs_test")

	err := gw.PlanGoal(nodeCtx(), "goal-1", "", []gateway.PlannedItem{{Seq: 1, Title: "docs", Model: "haiku"}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows := planned(t, f); len(rows) != 1 || rows[0]["model"] != nil {
		t.Fatalf("sent %v", rows)
	}
	if f.Goals["goal-1"]["status"] != "awaiting_approval" {
		t.Fatalf("goal is %v", f.Goals["goal-1"]["status"])
	}
}

// planned is the item rows of the one planGoal the fake accepted.
func planned(t *testing.T, f *gatewaytest.Fake) []map[string]any {
	t.Helper()
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("planGoal accepted %d times", len(calls))
	}
	var rows []map[string]any
	for _, r := range calls[0].Input["items"].([]any) {
		rows = append(rows, r.(map[string]any))
	}
	return rows
}
