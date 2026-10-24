package gateway_test

import (
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// runsheet ADR 056: a platform whose PlannedItemInput takes
// acceptanceChecks is sent each item's checks, as {kind, check, expected}.
func TestPlanGoalSendsAcceptanceChecks(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.ItemAcceptanceChecks = true
	f.Goals["goal-1"] = map[string]any{"id": "goal-1", "status": "planning"}
	gw := gateway.New(f.URL(), "rs_test")

	err := gw.PlanGoal(nodeCtx(), "goal-1", "", []gateway.PlannedItem{
		{Seq: 1, Title: "docs", Acceptance: "the README has a Usage section", AcceptanceChecks: []gateway.AcceptanceCheck{
			{Kind: "observable", Check: "README.md has a Usage section", Expected: "it names the three commands"},
		}},
		{Seq: 2, Title: "the rest", AcceptanceChecks: []gateway.AcceptanceCheck{
			{Kind: "test", Check: "go test ./internal/node/", Expected: "passes"},
			{Kind: "command", Check: "go vet ./...", Expected: "exit 0"},
		}},
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := planned(t, f)
	one, _ := rows[0]["acceptanceChecks"].([]any)
	if len(one) != 1 {
		t.Fatalf("item 1 sent %v", rows[0])
	}
	if c, _ := one[0].(map[string]any); c["kind"] != "observable" || c["check"] != "README.md has a Usage section" || c["expected"] != "it names the three commands" {
		t.Fatalf("item 1's check = %v", one[0])
	}
	two, _ := rows[1]["acceptanceChecks"].([]any)
	if len(two) != 2 {
		t.Fatalf("item 2 sent %v", rows[1])
	}
	if c, _ := two[1].(map[string]any); c["kind"] != "command" || c["check"] != "go vet ./..." || c["expected"] != "exit 0" {
		t.Fatalf("item 2's second check = %v", two[1])
	}
}

// A platform without acceptanceChecks (before ADR 056) is never sent the
// field: the prose acceptance stands alone and the plan is accepted.
func TestPlanGoalAcceptanceChecksOnlyWhereTaken(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Goals["goal-1"] = map[string]any{"id": "goal-1", "status": "planning"}
	gw := gateway.New(f.URL(), "rs_test")

	err := gw.PlanGoal(nodeCtx(), "goal-1", "", []gateway.PlannedItem{
		{Seq: 1, Title: "docs", Acceptance: "go test ./... passes", AcceptanceChecks: []gateway.AcceptanceCheck{{Kind: "test", Check: "go test ./...", Expected: "passes"}}},
	}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := planned(t, f)
	if _, sent := rows[0]["acceptanceChecks"]; sent {
		t.Fatalf("acceptanceChecks sent to a platform without it: %v", rows[0])
	}
	if rows[0]["acceptance"] != "go test ./... passes" {
		t.Fatalf("item 1 sent %v", rows[0])
	}
}

// Today's platform refuses an item with no checks, naming it — what
// every plan of 2026-10-24 was refused with until the node sent them.
func TestPlanGoalRefusedWithoutChecksOnTodaysPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.ItemAcceptanceChecks = true
	f.Goals["goal-1"] = map[string]any{"id": "goal-1", "status": "planning"}
	gw := gateway.New(f.URL(), "rs_test")

	err := gw.PlanGoal(nodeCtx(), "goal-1", "", []gateway.PlannedItem{{Seq: 1, Title: "docs", Acceptance: "the README says so"}}, 0, nil)
	if err == nil || !strings.Contains(err.Error(), "item 1 (docs): no acceptance: each item carries at least one {kind, check, expected}") {
		t.Fatalf("err = %v", err)
	}
	if f.Goals["goal-1"]["status"] != "planning" {
		t.Fatalf("goal is %v", f.Goals["goal-1"]["status"])
	}
}
