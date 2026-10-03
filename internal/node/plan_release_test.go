package node

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A fetch that fails is reported (adr-022): each failure releases the
// plan claim with the error as its note, the fake's goal carries it as
// planError and stays in planning, and no try is counted against
// MaxPlanAttempts — the platform holds the record now.
func TestPlannerFetchFailureReleasesThePlanClaim(t *testing.T) {
	f := widgetsAlone(t)
	var logs bytes.Buffer
	pl, ws, h, cfg := planTokenPlanner(f, t, &logs)
	ws.fetchErr = errors.New("gh repo clone acme/widgets: exit status 4")
	for range 2 {
		if err := pl.Plan(context.Background(), shipIt(), scopesOf(cfg)[0]); err == nil {
			t.Fatal("plan succeeded on a failing fetch")
		}
	}
	calls := f.CallsNamed("releasePlan")
	if len(calls) != 2 {
		t.Fatalf("releasePlan calls = %d, want 2: %+v\n%s", len(calls), calls, logs.String())
	}
	note, _ := calls[0].Input["note"].(string)
	if note != calls[1].Input["note"] || !strings.Contains(note, "acme/widgets") || !strings.Contains(note, "exit status 4") {
		t.Fatalf("notes = %q, %q: want identical, naming acme/widgets and the error", note, calls[1].Input["note"])
	}
	if calls[0].Input["aggregateId"] != goalID || calls[0].Input["nodeId"] != cfg.NodeID {
		t.Fatalf("release = %+v", calls[0].Input)
	}
	g := f.Goal(goalID)
	if g["planError"] != note || g["status"] != "planning" {
		t.Fatalf("goal planError %q status %v, want %q planning", g["planError"], g["status"], note)
	}
	if n := strings.Count(logs.String(), "goal ship-it: fetch failed: "+note+"; plan claim released"); n != 2 {
		t.Fatalf("release logged %d times, want 2:\n%s", n, logs.String())
	}
	if pl.attempts[goalID] != 0 {
		t.Fatalf("attempts = %d, want 0: a released plan counts no try", pl.attempts[goalID])
	}
	if len(h.jobs) != 0 {
		t.Fatalf("sessions = %d, want none", len(h.jobs))
	}
}

// widgetsAlone is planTokenSeed with acme/widgets the project's one
// repo and its plan token served.
func widgetsAlone(t *testing.T) *gatewaytest.Fake {
	f := planTokenSeed(t)
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.PlanGitHubTokens[goalID] = f.PlanGitHubTokens[goalID][:1]
	return f
}

// A plan token refused for a repo (item 2's path) releases the claim
// with a note naming the repo, before any fetch.
func TestPlannerRefusedTokenReleasesNamingTheRepo(t *testing.T) {
	f := planTokenSeed(t)
	f.PlanGitHubTokens[goalID] = f.PlanGitHubTokens[goalID][:1] // widgets alone
	var logs bytes.Buffer
	pl, ws, _, cfg := planTokenPlanner(f, t, &logs)
	err := pl.Plan(context.Background(), shipIt(), scopesOf(cfg)[0])
	var fe *PlanFetchError
	if !errors.As(err, &fe) || fe.Repo != "acme/gadgets" {
		t.Fatalf("plan error = %#v, want a PlanFetchError naming acme/gadgets", err)
	}
	calls := f.CallsNamed("releasePlan")
	if len(calls) != 1 || !strings.Contains(calls[0].Input["note"].(string), "acme/gadgets") {
		t.Fatalf("releasePlan calls = %+v, want one naming acme/gadgets", calls)
	}
	if len(ws.repoOps) != 0 {
		t.Fatalf("workspace calls %q, want none", ws.repoOps)
	}
	if pl.attempts[goalID] != 0 {
		t.Fatalf("attempts = %d, want 0", pl.attempts[goalID])
	}
}

// A platform without releasePlan: the try is counted as before and the
// fallback said once per run, however many failures.
func TestPlannerNoPlanReleaseCountsTheTry(t *testing.T) {
	f := widgetsAlone(t)
	f.Unsupported["releasePlan"] = true
	var logs bytes.Buffer
	pl, ws, _, cfg := planTokenPlanner(f, t, &logs)
	ws.fetchErr = errors.New("gh repo clone acme/widgets: exit status 4")
	for range 2 {
		if err := pl.Plan(context.Background(), shipIt(), scopesOf(cfg)[0]); err == nil {
			t.Fatal("plan succeeded on a failing fetch")
		}
	}
	if pl.attempts[goalID] != 2 {
		t.Fatalf("attempts = %d, want 2", pl.attempts[goalID])
	}
	if n := strings.Count(logs.String(), "takes no plan release"); n != 1 {
		t.Fatalf("fallback said %d times, want once:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "plan claim released") {
		t.Fatalf("no release on a platform without it:\n%s", logs.String())
	}
	if g := f.Goal(goalID); g["planError"] != nil {
		t.Fatalf("planError = %v, want none", g["planError"])
	}
}
