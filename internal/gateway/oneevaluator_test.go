package gateway_test

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// An item row with protectedFiles and protectedRulePaths decodes into
// WorkItem.ProtectedFiles and ProtectedRulePaths (goal
// one-evaluator-for-ordinary-work).
func TestWorkItemProtectedFiles(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.WorkItems["item-1"] = map[string]any{
		"id": "item-1", "orgId": orgID, "seq": 1, "status": "ready",
		"protectedFiles":     []any{"schema.sql"},
		"protectedRulePaths": []any{"db/**"},
	}
	gw := gateway.New(f.URL(), "rs_test")

	item, err := gw.WorkItem(nodeCtx(), "item-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(item.ProtectedFiles) != 1 || item.ProtectedFiles[0] != "schema.sql" {
		t.Fatalf("ProtectedFiles = %+v", item.ProtectedFiles)
	}
	if len(item.ProtectedRulePaths) != 1 || item.ProtectedRulePaths[0] != "db/**" {
		t.Fatalf("ProtectedRulePaths = %+v", item.ProtectedRulePaths)
	}
}

// A project row with protectedPaths decodes into Project.ProtectedPaths.
func TestProjectProtectedPaths(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Projects["p1"] = map[string]any{"id": "p1", "orgId": orgID, "slug": "widgets", "protectedPaths": []any{"infra/**"}}
	gw := gateway.New(f.URL(), "rs_test")

	p, err := gw.Project(nodeCtx(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ProtectedPaths) != 1 || p.ProtectedPaths[0] != "infra/**" {
		t.Fatalf("ProtectedPaths = %+v", p.ProtectedPaths)
	}
}

// With Fake.NoProtectedFields the item and project reads succeed with
// the fields dropped, each refused once and then read without them
// (the readItems/readProjects fallback, as readyAt's does).
func TestProtectedFieldsOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NoProtectedFields = true
	f.WorkItems["item-1"] = map[string]any{
		"id": "item-1", "orgId": orgID, "seq": 1, "status": "ready",
		"protectedFiles": []any{"schema.sql"}, "protectedRulePaths": []any{"db/**"},
	}
	f.Projects["p1"] = map[string]any{"id": "p1", "orgId": orgID, "slug": "widgets", "protectedPaths": []any{"infra/**"}}
	gw := gateway.New(f.URL(), "rs_test")
	ctx := nodeCtx()

	item, err := gw.WorkItem(ctx, "item-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(item.ProtectedFiles) != 0 || len(item.ProtectedRulePaths) != 0 {
		t.Fatalf("ProtectedFiles/ProtectedRulePaths not dropped: %+v", item)
	}
	if n := len(f.ReadsNamed("workItemIndex")); n != 2 {
		t.Fatalf("workItemIndex read %d times, want 2 (the refused selection, then the fallback)", n)
	}

	p, err := gw.Project(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ProtectedPaths) != 0 {
		t.Fatalf("ProtectedPaths not dropped: %+v", p)
	}
	if n := len(f.ReadsNamed("projectIndex")); n != 2 {
		t.Fatalf("projectIndex read %d times, want 2 (the refused selection, then the fallback)", n)
	}
}

// EvaluateWorkItem sends evaluatedModel when the choice names one, and
// the fake's item records it.
func TestEvaluateWorkItemSendsEvaluatedModel(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	evaluatingItem(f)
	gw := gateway.New(f.URL(), "rs_test")

	choice := gateway.EvaluateChoice{EvaluatedModel: "gpt-6-sol"}
	if err := gw.EvaluateWorkItem(nodeCtx(), finishItem, true, "looks good", nil, 50, nil, choice); err != nil {
		t.Fatal(err)
	}
	in := f.CallsNamed("evaluateWorkItem")[0].Input
	if in[gateway.EvaluatedModelField] != "gpt-6-sol" {
		t.Fatalf("input = %+v", in)
	}
	if row := f.Item(finishItem); row["evaluatedModel"] != "gpt-6-sol" {
		t.Fatalf("row = %+v", row)
	}
}

// A zero EvaluateChoice sends no evaluatedModel, as EvaluateWorkItem and
// EvaluateWorkItemNoVerdict always did before this goal.
func TestEvaluateWorkItemWithoutEvaluatedModel(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	evaluatingItem(f)
	gw := gateway.New(f.URL(), "rs_test")

	if err := gw.EvaluateWorkItem(nodeCtx(), finishItem, true, "looks good", nil, 50, nil, gateway.EvaluateChoice{}); err != nil {
		t.Fatal(err)
	}
	in := f.CallsNamed("evaluateWorkItem")[0].Input
	if _, sent := in[gateway.EvaluatedModelField]; sent {
		t.Fatalf("evaluatedModel sent with a zero choice: %+v", in)
	}
}
