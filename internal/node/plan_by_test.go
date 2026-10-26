package node

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// The refusal loop of 2026-10-26 (runsheet ADR 070): the platform began
// refusing a machine bearer that names anyone as `by`, and the node's
// planner still named its operator on planGoal and setGoalCheckpoints.
// Every plan was refused, and a fresh Planner per job planned the same
// goals again on every tick — 869 sessions in six hours.

// On a machine bearer, the plan and its `pr` checkpoint land: neither
// names a `by`, though the node's config has an owner.
func TestPlannerHandInNamesNoBy(t *testing.T) {
	f := seed(t)
	f.NodeCredentialID = "33333333-3333-4333-8333-333333333333" // the bearer is a machine
	f.ItemPrecondition = true
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "44444444-4444-4444-8444-444444444444", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	var logs bytes.Buffer
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: &fakeHarness{texts: []string{cutOverSecond}}, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logs, "", 0)}
	goal := gateway.Goal{ID: goalID, Slug: "release-tag-mirrors", Title: "Release tags mirror", ProjectID: projID, Status: "planning"}
	if err := pl.Plan(context.Background(), goal, scopesOf(cfg)[0]); err != nil {
		t.Fatalf("plan: %v\n%s", err, logs.String())
	}
	for _, name := range []string{"setGoalCheckpoints", "planGoal"} {
		calls := f.CallsNamed(name)
		if len(calls) != 1 {
			t.Fatalf("%s calls = %d, want 1", name, len(calls))
		}
		if by, sent := calls[0].Input["by"]; sent {
			t.Errorf("%s named by %v", name, by)
		}
	}
	if strings.Contains(logs.String(), "refused") {
		t.Errorf("log says refused:\n%s", logs.String())
	}
}

// A hand-in the platform refuses is a refused plan: the same refusal
// twice running releases the claim, and the goal is not planned again
// by the next job's fresh Planner until its row changes.
func TestPlanHandInRefusedTwiceIsReleased(t *testing.T) {
	f := seedLoom(t)
	refusal := "planGoal: a machine names no person as by: not yours"
	f.Refuse["planGoal"] = refusal
	gw := gateway.New(f.URL(), "rs_test")
	plan := "```json\n{\"items\": [{\"seq\": 2, \"title\": \"Serve the looms over gRPC with mTLS\", \"brief\": \"The server, with mTLS.\", \"acceptance\": \"`go test ./internal/server/...` passes.\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"
	h := &fakeHarness{text: plan}
	root := t.TempDir()
	var logged strings.Builder

	for i := range 4 { // `mesh node start` builds a Planner per plan
		pl, cfg := loomPlanner(gw, h, root, &logged)
		err := requestPlan(t, gw, pl, cfg)
		if i == 0 {
			if pr, ok := PlanRefusedOf(err); !ok || pr.Released || !strings.Contains(pr.Reason, refusal) {
				t.Fatalf("first plan = %#v, want a *PlanRefused naming %q, not released", err, refusal)
			}
		}
	}
	if len(h.jobs) != 2 {
		t.Fatalf("plan sessions = %d, want 2\n%s", len(h.jobs), logged.String())
	}
	if len(f.PlanReleases) != 1 || !strings.Contains(f.PlanReleases[0].Reason, refusal) {
		t.Fatalf("plan releases = %+v, want one naming %q", f.PlanReleases, refusal)
	}
	if !strings.Contains(logged.String(), "goal loom: plan refused twice with the same reason; released, not re-planned: hand in the plan: ") {
		t.Fatalf("log lacks the release line:\n%s", logged.String())
	}
}

// An error that is not the platform's answer — the gateway unreachable —
// stays an error, a try counted, not a refusal that releases the claim.
func TestPlanHandInTransportErrorIsNotARefusal(t *testing.T) {
	err := planHandInRefused(gateway.Goal{Slug: "g"}, "hand in the plan", context.DeadlineExceeded)
	if _, ok := PlanRefusedOf(err); ok {
		t.Fatalf("a transport error became a refusal: %v", err)
	}
	if err.Error() != "hand in the plan: context deadline exceeded" {
		t.Fatalf("err = %q", err)
	}
}
