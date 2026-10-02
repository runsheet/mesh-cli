package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A platform whose ProjectIndex has no secondOpinion (today's platform) is
// read without error, and the evaluate path's own rule applies
// (SecondOpinionRule "rule"): goal one-evaluator-for-ordinary-work.
func TestProjectWithoutSecondOpinionOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields["secondOpinion"] = true
	f.Projects["p1"] = map[string]any{"id": "p1", "orgId": orgID, "slug": "widgets"}
	gw := gateway.New(f.URL(), "rs_test")

	p, err := gw.Project(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if p.SecondOpinionRule() != "rule" {
		t.Fatalf("SecondOpinionRule() = %q, want rule", p.SecondOpinionRule())
	}
}

// SecondOpinionRule is "always" only for exactly "always", else "rule".
// The gateway read of a row with secondOpinion "always" waits on the
// platform: ProjectIndex does not serve secondOpinion yet, and ADR-010
// lets fields/ProjectIndex.txt select it only once schema.json is
// refreshed from a gateway that does.
func TestSecondOpinionRule(t *testing.T) {
	for in, want := range map[string]string{"": "rule", "rule": "rule", "always": gateway.SecondOpinionAlways, "Always": "rule", "never": "rule"} {
		if got := (gateway.Project{SecondOpinion: in}).SecondOpinionRule(); got != want {
			t.Errorf("SecondOpinionRule(%q) = %q, want %q", in, got, want)
		}
	}
}

// An item row with protectedFiles decodes into WorkItem.ProtectedFiles
// (runsheet ADR 021, console-trd §3).
func TestWorkItemProtectedFiles(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.WorkItems["item-1"] = map[string]any{
		"id": "item-1", "orgId": orgID, "seq": 1, "status": "ready",
		"protectedFiles":     []any{"schema.sql"},
		"protectedRulePaths": []any{"**/schema.sql"},
	}
	gw := gateway.New(f.URL(), "rs_test")

	item, err := gw.WorkItem(context.Background(), "item-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(item.ProtectedFiles) != 1 || item.ProtectedFiles[0] != "schema.sql" {
		t.Fatalf("ProtectedFiles = %+v", item.ProtectedFiles)
	}
	if len(item.ProtectedRulePaths) != 1 || item.ProtectedRulePaths[0] != "**/schema.sql" {
		t.Fatalf("ProtectedRulePaths = %+v", item.ProtectedRulePaths)
	}
}

// EvaluateWorkItem always sends evaluatedModel when the choice names one.
// evaluators/evaluatorsWhy go only to a gateway whose EvaluateWorkItemInput
// takes them (Fake.EvaluatorsField); a platform without them still takes
// the verdict, just without the count and the reason.
func TestEvaluateWorkItemSendsEvaluatedModelAndEvaluators(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	evaluatingItem(f)
	f.EvaluatorsField = true
	gw := gateway.New(f.URL(), "rs_test")

	choice := gateway.EvaluateChoice{Model: "opus", Evaluators: 2, Why: "protected path"}
	if err := gw.EvaluateWorkItem(context.Background(), finishItem, true, "looks good", nil, 50, nil, choice); err != nil {
		t.Fatal(err)
	}
	in := f.CallsNamed("evaluateWorkItem")[0].Input
	if in["evaluatedModel"] != "opus" || in["evaluators"] != float64(2) || in["evaluatorsWhy"] != "protected path" {
		t.Fatalf("input = %+v", in)
	}
	if row := f.Item(finishItem); row["evaluatedModel"] != "opus" || row["evaluators"] != float64(2) || row["evaluatorsWhy"] != "protected path" {
		t.Fatalf("row = %+v", row)
	}
}

// A platform whose EvaluateWorkItemInput lacks evaluators (today's
// platform, Fake.EvaluatorsField false) takes the verdict without them —
// evaluatedModel still sent, the mutation still succeeds.
func TestEvaluateWorkItemWithoutEvaluatorsOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	evaluatingItem(f)
	gw := gateway.New(f.URL(), "rs_test")

	choice := gateway.EvaluateChoice{Model: "opus", Evaluators: 2, Why: "protected path"}
	if err := gw.EvaluateWorkItem(context.Background(), finishItem, true, "looks good", nil, 50, nil, choice); err != nil {
		t.Fatal(err)
	}
	in := f.CallsNamed("evaluateWorkItem")[0].Input
	if in["evaluatedModel"] != "opus" {
		t.Fatalf("evaluatedModel not sent: %+v", in)
	}
	if _, sent := in["evaluators"]; sent {
		t.Fatalf("evaluators sent to a platform without the field: %+v", in)
	}
	if _, sent := in["evaluatorsWhy"]; sent {
		t.Fatalf("evaluatorsWhy sent to a platform without the field: %+v", in)
	}
}
