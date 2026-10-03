package node

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const repo2ID = "abababab-4444-4444-8444-abababababab"

// planTokenSeed is seed with a second repo, acme/gadgets, enrolled in
// the project beside acme/widgets, the goal in planning, and the goal's
// plan tokens served for both (adr-022).
func planTokenSeed(t *testing.T) *gatewaytest.Fake {
	t.Helper()
	f := seed(t)
	delete(f.Unsupported, "planGitHubToken")
	f.Repos[repo2ID] = map[string]any{"id": repo2ID, "accountId": acctID, "name": "gadgets", "defaultBranch": "main"}
	f.Projects[projID]["repoIds"] = []string{repoID, repo2ID}
	f.Goals[goalID]["status"] = "planning"
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	f.PlanGitHubTokens[goalID] = []map[string]any{
		{"token": "ghs_plan_widgets", "expiresAt": exp, "repository": "acme/widgets"},
		{"token": "ghs_plan_gadgets", "expiresAt": exp, "repository": "acme/gadgets"},
	}
	return f
}

const planTokenJSON = "```json\n{\"items\": [{\"seq\": 1, \"title\": \"Do it\", \"brief\": \"All of it.\", \"acceptance\": \"go test ./... passes\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"

func planTokenPlanner(f *gatewaytest.Fake, t *testing.T, logs *bytes.Buffer) (*Planner, *fakeWS, *fakeHarness, *Config) {
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: planTokenJSON}
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(logs, "", 0)}
	return pl, ws, h, cfg
}

func shipIt() gateway.Goal {
	return gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}
}

// A node with no gh login plans: each repo's checkout is authorized on
// its own row of the goal's plan tokens before it is fetched, and the
// session runs with GH_TOKEN the first repo's (by sorted name) and a
// GH_CONFIG_DIR of its own, never the operator's (adr-022).
func TestPlannerFetchesOnTheGoalsTokens(t *testing.T) {
	// the operator never ran gh auth login: an empty gh config, no token
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_TOKEN", "")
	os.Unsetenv("GH_TOKEN")
	f := planTokenSeed(t)
	var logs bytes.Buffer
	pl, ws, h, cfg := planTokenPlanner(f, t, &logs)
	if err := pl.Plan(context.Background(), shipIt(), scopesOf(cfg)[0]); err != nil {
		t.Fatalf("plan: %v\n%s", err, logs.String())
	}

	dir := PlanDir(ws.root, "ship-it")
	wantOps := []string{"authorize acme/gadgets ghs_plan_gadgets", "fetch acme/gadgets", "authorize acme/widgets ghs_plan_widgets", "fetch acme/widgets"}
	if !slices.Equal(ws.repoOps, wantOps) {
		t.Fatalf("workspace calls = %q, want %q", ws.repoOps, wantOps)
	}
	wantAuth := []string{
		"acme/gadgets ghs_plan_gadgets " + filepath.Join(dir, "repos", "gadgets"),
		"acme/widgets ghs_plan_widgets " + filepath.Join(dir, "repos", "widgets"),
	}
	if !slices.Equal(ws.authorized, wantAuth) {
		t.Fatalf("authorized = %q, want %q", ws.authorized, wantAuth)
	}
	if len(f.CallsNamed("planGitHubToken")) != 1 {
		t.Fatalf("planGitHubToken calls: %+v", f.CallsNamed("planGitHubToken"))
	}
	if calls := f.CallsNamed("planGoal"); len(calls) != 1 {
		t.Fatalf("planGoal calls: %+v", calls)
	}

	if len(h.jobs) != 1 {
		t.Fatalf("planner sessions = %d, want 1", len(h.jobs))
	}
	operator, err := OperatorGHConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	var token, ghDir string
	for _, kv := range h.jobs[0].Env {
		if v, ok := strings.CutPrefix(kv, "GH_TOKEN="); ok {
			token = v
		}
		if v, ok := strings.CutPrefix(kv, "GH_CONFIG_DIR="); ok {
			ghDir = v
		}
	}
	if token != "ghs_plan_gadgets" {
		t.Fatalf("session GH_TOKEN = %q, want the first sorted repo's, ghs_plan_gadgets (env %q)", token, h.jobs[0].Env)
	}
	if ghDir == "" || ghDir == operator {
		t.Fatalf("session GH_CONFIG_DIR = %q, operator's %q", ghDir, operator)
	}
	if strings.Contains(logs.String(), "operator's gh") {
		t.Fatalf("no fallback on a platform that issues plan tokens:\n%s", logs.String())
	}
}

// A repo the platform issues no token for is the plan's error, naming
// the repo, before any fetch and before any session.
func TestPlannerRefusedTokenNamesTheRepo(t *testing.T) {
	f := planTokenSeed(t)
	f.PlanGitHubTokens[goalID] = f.PlanGitHubTokens[goalID][:1] // widgets alone
	var logs bytes.Buffer
	pl, ws, h, cfg := planTokenPlanner(f, t, &logs)
	err := pl.Plan(context.Background(), shipIt(), scopesOf(cfg)[0])
	if err == nil || !strings.Contains(err.Error(), "acme/gadgets") {
		t.Fatalf("plan error = %v, want one naming acme/gadgets", err)
	}
	if len(h.jobs) != 0 || len(ws.repoOps) != 0 {
		t.Fatalf("sessions %d, workspace calls %q: want none", len(h.jobs), ws.repoOps)
	}
	if calls := f.CallsNamed("planGoal"); len(calls) != 0 {
		t.Fatalf("planGoal calls: %+v", calls)
	}
}

// An old platform plans as before, on the operator's gh: Fetch without
// Authorize, no GH_TOKEN on the session, the fallback said once for the
// node's run however many goals it plans.
func TestPlannerOldPlatformFallsBackOnce(t *testing.T) {
	f := planTokenSeed(t)
	f.Unsupported["planGitHubToken"] = true
	const goal2 = "dddddddd-3333-4333-8333-dddddddddddd"
	f.Goals[goal2] = map[string]any{"id": goal2, "orgId": orgID, "projectId": projID, "slug": "new-thing", "title": "New thing",
		"statement": "Build the new thing.", "acceptance": "the thing works", "budgetCents": 2000, "spentCents": 0, "status": "planning"}
	var logs bytes.Buffer
	pl, ws, h, cfg := planTokenPlanner(f, t, &logs)
	for _, g := range []gateway.Goal{shipIt(), {ID: goal2, Slug: "new-thing", Title: "New thing", ProjectID: projID, Status: "planning"}} {
		if err := pl.Plan(context.Background(), g, scopesOf(cfg)[0]); err != nil {
			t.Fatalf("plan %s: %v", g.Slug, err)
		}
	}
	if len(ws.authorized) != 0 || len(ws.fetched) != 4 {
		t.Fatalf("authorized %q, fetched %q: want 4 fetches and no Authorize", ws.authorized, ws.fetched)
	}
	if n := strings.Count(logs.String(), "platform issues no plan token; fetching on the operator's gh"); n != 1 {
		t.Fatalf("fallback said %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "goal ship-it: platform issues no plan token; fetching on the operator's gh") {
		t.Fatalf("fallback line:\n%s", logs.String())
	}
	for _, j := range h.jobs {
		for _, kv := range j.Env {
			if strings.HasPrefix(kv, "GH_TOKEN=") {
				t.Fatalf("an old platform's plan session got %q", kv)
			}
		}
	}
	if calls := f.CallsNamed("planGoal"); len(calls) != 2 {
		t.Fatalf("planGoal calls: %d, want 2", len(calls))
	}
}
