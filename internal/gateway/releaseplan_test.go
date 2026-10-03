package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const releasePlanNote = "fetch acme/widgets: gh repo clone acme/widgets: exit status 4"

// ReleasePlan sends releasePlan on the goal with nodeId and note; the
// fake clears the claim, keeps the goal in planning and records the
// note as its planError (adr-022, A fetch that fails is reported).
func TestReleasePlan(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	goalID := planGoalID()
	f.Goals[goalID] = map[string]any{"id": goalID, "projectId": projID, "status": "planning", "plannerNodeId": nodeID, "planClaimedAt": "2026-10-03T05:11:00Z"}
	gw := gateway.New(f.URL(), "rs_test")

	if err := gw.ReleasePlan(context.Background(), goalID, nodeID, releasePlanNote); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("releasePlan")
	if len(calls) != 1 || calls[0].Input["aggregateId"] != goalID ||
		calls[0].Input["nodeId"] != nodeID || calls[0].Input["note"] != releasePlanNote {
		t.Fatalf("calls = %+v", calls)
	}
	g := f.Goal(goalID)
	if g["status"] != "planning" || g["planError"] != releasePlanNote || g["plannerNodeId"] != nil || g["planClaimedAt"] != nil {
		t.Fatalf("goal = %+v", g)
	}
}

// A platform without releasePlan answers IsUnsupported.
func TestReleasePlanUnsupported(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Unsupported["releasePlan"] = true
	goalID := planGoalID()
	f.Goals[goalID] = map[string]any{"id": goalID, "projectId": projID, "status": "planning", "plannerNodeId": nodeID}
	gw := gateway.New(f.URL(), "rs_test")

	err := gw.ReleasePlan(context.Background(), goalID, nodeID, releasePlanNote)
	if !gateway.IsUnsupported(err) {
		t.Fatalf("err = %v, want IsUnsupported", err)
	}
	if g := f.Goal(goalID); g["planError"] != nil || g["plannerNodeId"] != nodeID {
		t.Fatalf("goal changed: %+v", g)
	}
}
