package gateway_test

import (
	"context"
	"encoding/json"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A project with no MaxItems set (an older platform, today every
// platform: ProjectIndex.maxItems is not yet in fields/ProjectIndex.txt
// because the live gateway does not serve it — see client.go's comment
// on Project.MaxItems) caps a plan at DefaultMaxItems.
func TestItemCapDefaultsToEight(t *testing.T) {
	if gateway.DefaultMaxItems != 8 {
		t.Fatalf("DefaultMaxItems = %d, want 8", gateway.DefaultMaxItems)
	}
	if got := (gateway.Project{}).ItemCap(); got != 8 {
		t.Fatalf("ItemCap() of a zero-value project = %d, want 8", got)
	}
}

// A project with its own MaxItems overrides the default.
func TestItemCapHonoursTheProjectsOwn(t *testing.T) {
	if got := (gateway.Project{MaxItems: 5}).ItemCap(); got != 5 {
		t.Fatalf("ItemCap() = %d, want 5", got)
	}
}

// Project.MaxItems decodes a served `maxItems` by its json tag. This is
// the decode path alone: the live gateway does not yet serve
// ProjectIndex.maxItems (see client.go's comment on Project.MaxItems
// and ADR-023's amendment), so fields/ProjectIndex.txt has no
// `maxItems?` yet and the Client's own project query never asks for
// it — once it does, this is the shape the answer arrives in.
func TestProjectMaxItemsDecodesAServedValue(t *testing.T) {
	var p gateway.Project
	if err := json.Unmarshal([]byte(`{"maxItems": 5}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.MaxItems != 5 {
		t.Fatalf("MaxItems = %d, want 5", p.MaxItems)
	}
	if got := p.ItemCap(); got != 5 {
		t.Fatalf("ItemCap() = %d, want 5", got)
	}

	// An older platform (no maxItems in the response) decodes to the
	// zero value, and ItemCap falls back to DefaultMaxItems.
	var p2 gateway.Project
	if err := json.Unmarshal([]byte(`{"id": "p1"}`), &p2); err != nil {
		t.Fatal(err)
	}
	if got := p2.ItemCap(); got != gateway.DefaultMaxItems {
		t.Fatalf("ItemCap() = %d, want DefaultMaxItems", got)
	}
}

func splitGoalID() string { return "dddddddd-9999-4999-8999-dddddddddddd" }

// ProposeGoalSplit sends the drafts and costCents to a platform whose
// Mutation has proposeGoalSplit (ADR-023, the size rule), and the fake
// records the call.
func TestProposeGoalSplitSendsDraftsAndCost(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.GoalSplit = true
	goalID := splitGoalID()
	f.Goals[goalID] = map[string]any{"id": goalID, "projectId": projID}
	gw := gateway.New(f.URL(), "rs_test")

	drafts := []gateway.GoalDraft{
		{Slug: "first-half", Title: "First half", Statement: "...", Acceptance: "...", RepoID: "repo-1"},
		{Slug: "second-half", Title: "Second half", Statement: "...", Acceptance: "...", RepoID: "repo-1"},
	}
	if err := gw.ProposeGoalSplit(context.Background(), goalID, "node-1", drafts, 150, nil); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("proposeGoalSplit")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	in := calls[0].Input
	if in["costCents"] != float64(150) && in["costCents"] != 150 {
		t.Fatalf("costCents = %v", in["costCents"])
	}
	rows, _ := in["drafts"].([]any)
	if len(rows) != 2 {
		t.Fatalf("drafts = %+v", in["drafts"])
	}
	row, _ := rows[0].(map[string]any)
	if row["slug"] != "first-half" || row["title"] != "First half" || row["statement"] != "..." ||
		row["acceptance"] != "..." || row["repoId"] != "repo-1" {
		t.Fatalf("draft row = %+v", row)
	}
}

// TakesGoalSplit is false against a fake whose Mutation lacks
// proposeGoalSplit (the platform as it is today) and true once the
// fake simulates the platform shipping it.
func TestTakesGoalSplit(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	gw := gateway.New(f.URL(), "rs_test")
	if gw.TakesGoalSplit(context.Background()) {
		t.Fatal("TakesGoalSplit = true against a fake without GoalSplit")
	}

	f2 := gatewaytest.New("rs_test")
	defer f2.Close()
	f2.GoalSplit = true
	gw2 := gateway.New(f2.URL(), "rs_test")
	if !gw2.TakesGoalSplit(context.Background()) {
		t.Fatal("TakesGoalSplit = false against a fake with GoalSplit")
	}
}

// ProposeGoalSplit against a platform without the mutation is
// IsUnsupported, and nothing is sent.
func TestProposeGoalSplitUnsupportedOnOldPlatform(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	goalID := splitGoalID()
	f.Goals[goalID] = map[string]any{"id": goalID, "projectId": projID}
	gw := gateway.New(f.URL(), "rs_test")

	err := gw.ProposeGoalSplit(context.Background(), goalID, "node-1", []gateway.GoalDraft{{Slug: "a"}}, 100, nil)
	if !gateway.IsUnsupported(err) {
		t.Fatalf("err = %v, want unsupported", err)
	}
	if calls := f.CallsNamed("proposeGoalSplit"); len(calls) != 0 {
		t.Fatalf("calls = %+v, want none sent", calls)
	}
}

// SetGoalCheckpoints sends the checkpoints list on the live mutation.
func TestSetGoalCheckpointsSendsTheList(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	goalID := splitGoalID()
	f.Goals[goalID] = map[string]any{"id": goalID, "projectId": projID}
	gw := gateway.New(f.URL(), "rs_test")

	checkpoints := []string{"pr: a person confirms the rollout in production"}
	if err := gw.SetGoalCheckpoints(context.Background(), goalID, "node-1", checkpoints); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("setGoalCheckpoints")
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	cps, _ := calls[0].Input["checkpoints"].([]any)
	if len(cps) != 1 || cps[0] != checkpoints[0] {
		t.Fatalf("checkpoints = %+v", calls[0].Input["checkpoints"])
	}
	if got, _ := f.Goals[goalID]["checkpoints"].([]any); len(got) != 1 || got[0] != checkpoints[0] {
		t.Fatalf("goal row checkpoints = %+v", f.Goals[goalID]["checkpoints"])
	}
}
