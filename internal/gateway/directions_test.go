package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const (
	orgID   = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	projID  = "cccccccc-3333-4333-8333-cccccccccccc"
	otherID = "cccccccc-4444-4444-8444-cccccccccccc"
	nodeID  = "22222222-2222-4222-8222-222222222222"
)

func directionRow(id, projectID, slug string) map[string]any {
	return map[string]any{
		"id": id, "projectId": projectID, "orgId": orgID, "slug": slug, "name": slug,
		"direction": "Go on then.", "enabled": true, "fromMinute": 1320, "toMinute": 420,
		"timezone": "Australia/Brisbane", "gapMinutes": 45, "maxLiveGoals": 2, "status": "active",
	}
}

// The node lists a project's directions — org-scoped as the gateway
// requires, narrowed on the project — and every field it needs to
// decide whether to offer one comes back on the row.
func TestDirections(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Directions["d1"] = directionRow("d1", projID, "overnight")
	f.Directions["d2"] = directionRow("d2", projID, "mornings")
	f.Directions["d3"] = directionRow("d3", otherID, "elsewhere")
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	all, err := gw.Directions(ctx, orgID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("the org's directions: %+v", all)
	}
	mine, err := gw.Directions(ctx, orgID, projID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 {
		t.Fatalf("the project's directions: %+v", mine)
	}
	var d gateway.Direction
	for _, row := range mine {
		if row.Slug == "overnight" {
			d = row
		}
		if row.ProjectID != projID {
			t.Fatalf("narrowed on the project: %+v", row)
		}
	}
	if d.ID != "d1" || d.OrgID != orgID || d.Direction != "Go on then." || !d.Enabled {
		t.Fatalf("the words and the switch: %+v", d)
	}
	if d.FromMinute != 1320 || d.ToMinute != 420 || d.Timezone != "Australia/Brisbane" {
		t.Fatalf("the window, in the direction's own zone: %+v", d)
	}
	if d.GapMinutes != 45 || d.MaxLiveGoals != 2 || d.Status != "active" {
		t.Fatalf("the pace, the share and the status: %+v", d)
	}
	if !d.Active() {
		t.Fatal("an enabled, unarchived direction is one to offer")
	}
	d.Status = "archived"
	if d.Active() {
		t.Fatal("an archived direction is not offered at all")
	}
}

// The claim and the hand-in are against the direction's own aggregate,
// and the platform's "not now" is a refusal — the node's ordinary
// answer, not an error to shout about.
func TestDirectionResearchClaimAndRecord(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Directions["d1"] = directionRow("d1", projID, "overnight")
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	if err := gw.ClaimDirectionResearch(ctx, "d1", nodeID); err != nil {
		t.Fatal(err)
	}
	claim := f.CallsNamed("claimDirectionResearch")
	if len(claim) != 1 || claim[0].Input["aggregateId"] != "d1" || claim[0].Input["nodeId"] != nodeID {
		t.Fatalf("claimed against the direction: %+v", claim)
	}
	if at, _ := claim[0].Input["at"].(string); at == "" {
		t.Fatal("a claim carries the time it is made: the window is judged on it")
	}

	proposals := []gateway.GoalProposal{
		{Slug: "one-sign-in", Title: "One sign-in", Statement: "Everyone signs in once.",
			Acceptance: "a person signs in once", Why: "the direction asks for it", BudgetCents: 3000},
	}
	if err := gw.RecordDirectionResearch(ctx, "d1", nodeID, proposals, 125, "1 proposed", nil); err != nil {
		t.Fatal(err)
	}
	rec := f.CallsNamed("recordDirectionResearch")
	if len(rec) != 1 || rec[0].Input["aggregateId"] != "d1" || rec[0].Input["costCents"] != float64(125) {
		t.Fatalf("handed in against the direction, with the cost: %+v", rec)
	}
	list, _ := rec[0].Input["proposals"].([]any)
	if len(list) != 1 {
		t.Fatalf("proposals: %+v", list)
	}
	first, _ := list[0].(map[string]any)
	if first["slug"] != "one-sign-in" || first["budgetCents"] != float64(3000) {
		t.Fatalf("proposal: %+v", first)
	}
	if held, _ := f.Directions["d1"]["researchNodeId"].(string); held != "" {
		t.Fatalf("the hand-in lets the claim go: %q", held)
	}

	// outside the window, inside the gap, or held elsewhere: all the
	// same shape, and all IsRefusal
	f.Refuse["claimDirectionResearch"] = "the direction is outside its window"
	err := gw.ClaimDirectionResearch(ctx, "d1", nodeID)
	if err == nil || !gateway.IsRefusal(err) {
		t.Fatalf("a refused claim is a refusal, not a failure: %v", err)
	}
}

// A direction's mode rides the row (runsheet ADR 022): propose is one
// whose research only proposes; set, or none, is guarded by the room.
func TestDirectionsReadTheMode(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Directions["d1"] = directionRow("d1", projID, "scout")
	f.Directions["d1"]["mode"] = "propose"
	f.Directions["d2"] = directionRow("d2", projID, "overnight")
	f.Directions["d2"]["mode"] = "set"
	gw := gateway.New(f.URL(), "rs_test")

	got, err := gw.Directions(context.Background(), orgID, projID)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]gateway.Direction{}
	for _, d := range got {
		modes[d.Slug] = d
	}
	if d := modes["scout"]; d.Mode != "propose" || !d.Proposes() {
		t.Fatalf("a propose direction: %+v", d)
	}
	if d := modes["overnight"]; d.Mode != "set" || d.Proposes() {
		t.Fatalf("a set direction: %+v", d)
	}
	if (gateway.Direction{}).Proposes() {
		t.Fatal("a direction with no mode is guarded, not proposing")
	}
}

// A platform older than ADR 022 refuses mode: the read falls back to
// the required fields, still returns the rows — with no mode, so
// guarded — and stops asking for it.
func TestDirectionsWithoutModeOnAnOlderPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.UnknownFields["mode"] = true
	f.Directions["d1"] = directionRow("d1", projID, "scout")
	gw := gateway.New(f.URL(), "rs_test")
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		got, err := gw.Directions(ctx, orgID, projID)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if len(got) != 1 || got[0].ID != "d1" || got[0].Mode != "" || got[0].Proposes() {
			t.Fatalf("read %d: %+v", i, got)
		}
	}
	// the first read tried mode once; every read after asks without
	if n := len(f.ReadsNamed("directionIndexs")); n != 3 {
		t.Fatalf("directionIndexs read %d times, want 3", n)
	}
}
