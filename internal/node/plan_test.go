package node

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// A goal in planning gets the planner: the project's repos fetched
// read-only, the context written, a fresh session whose JSON graph is
// handed in with its cost; a session that writes no graph hands in
// nothing, three times, then is left alone.
func TestLoopPlansRequestedGoals(t *testing.T) {
	f := seed(t)
	const goal2 = "dddddddd-3333-4333-8333-dddddddddddd"
	f.Goals[goal2] = map[string]any{"id": goal2, "orgId": orgID, "projectId": projID, "slug": "new-thing", "title": "New thing",
		"statement": "Build the new thing.", "acceptance": "the thing works", "budgetCents": 2000, "spentCents": 0, "status": "planning"}
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "I read the repo and the PRD.\n\n```json\n{\"items\": [" +
		"{\"seq\": 1, \"title\": \"Write the failing test\", \"brief\": \"A test in widgets.\", \"acceptance\": \"go test fails on TestThing\", \"depends_on\": [], \"repo\": \"acme/widgets\"}," +
		"{\"seq\": 2, \"title\": \"Make it pass\", \"brief\": \"Implement Thing.\", \"acceptance\": \"go test ./... passes\", \"depends_on\": [{\"seq\": 1, \"because\": \"uses TestThing item 1 adds\"}], \"repo\": \"widgets\", \"base_ref\": \"main\"}" +
		"]}\n```"}
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 4, Log: log.New(os.Stderr, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: pl.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Plan:     func(ctx context.Context, g gateway.Goal, sc Scope) { _ = pl.Plan(ctx, g, sc) }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()

	if claims := f.CallsNamed("claimPlan"); len(claims) != 1 || claims[0].Input["nodeId"] != cfg.NodeID {
		t.Fatalf("claim calls: %+v", claims)
	}
	// the session hands its plan to the product's own tool; the node
	// hands in nothing of it, and closes the job with an empty result
	if n := len(f.CallsNamed("planGoal")); n != 0 {
		t.Fatalf("the node handed a plan in itself %d times", n)
	}
	fin := f.CallsNamed("finishJob")
	if len(fin) != 1 || fin[0].Input["kind"] != PlanKind || fin[0].Input["nodeId"] != cfg.NodeID {
		t.Fatalf("finishJob calls: %+v", fin)
	}
	if res, ok := fin[0].Input["finish"].(map[string]any)["result"].(map[string]any); !ok || len(res) != 0 {
		t.Fatalf("a plan job closes with an empty result: %+v", fin[0].Input["finish"])
	}
	// the running goal was not planned; the session saw the repo and the context
	if len(h.jobs) != 1 || h.jobs[0].Kind != harness.KindPlan || h.jobs[0].MaxBudgetUSD != 4 || h.jobs[0].Workspace != PlanDir(ws.root, "new-thing") {
		t.Fatalf("job: %+v", h.jobs)
	}
	if len(ws.fetched) != 1 || !strings.HasPrefix(ws.fetched[0], "acme/widgets → "+filepath.Join(PlanDir(ws.root, "new-thing"), "repos", "widgets")) {
		t.Fatalf("fetched: %v", ws.fetched)
	}
	p := h.jobs[0].Prompt
	for _, want := range []string{"Build the new thing.", "the thing works", "$20.00", "`acme/widgets` in `repos/widgets/`", ContextDir, "instructions-acme-widgets-agents.md", "Hand your result in with `mesh_plan` on the runsheet MCP server."} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if len(ws.pushed) != 0 {
		t.Fatal("the planner changes nothing")
	}

	// a goal another node is planning is left alone
	const goal3 = "dddddddd-4444-4444-8444-dddddddddddd"
	f.Goals[goal3] = map[string]any{"id": goal3, "orgId": orgID, "projectId": projID, "slug": "theirs", "title": "Theirs", "status": "planning", "plannerNodeId": "33333333-3333-4333-8333-333333333333"}
	loop.Tick(ctx)
	loop.wg.Wait()
	if len(h.jobs) != 1 || len(f.CallsNamed("finishJob")) != 1 {
		t.Fatalf("a goal claimed elsewhere was planned: jobs=%d", len(h.jobs))
	}
	delete(f.Goals, goal3)

	// the product refuses to close a job no plan reached: the try counts,
	// and after MaxPlanAttempts the planner stops
	f.PlanNotHandedIn = "call mesh_plan first"
	f.Goals[goal2]["status"] = "planning"
	for i := 0; i < 5; i++ {
		loop.Tick(ctx)
		loop.wg.Wait()
	}
	if len(h.jobs) != 1+MaxPlanAttempts {
		t.Fatalf("planner sessions = %d, want %d", len(h.jobs), 1+MaxPlanAttempts)
	}
}

// On a re-plan the prompt lists the items so far, starts the numbering
// after them, and a dependency on a merged seq is accepted.
func TestPlannerReplans(t *testing.T) {
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Goals[goalID]["pausedReason"] = "item failed: evaluation failed 3 times"
	f.WorkItems[item1]["status"] = "merged"
	f.WorkItems[item1]["branch"] = "mesh/ship-it-1"
	f.WorkItems[item1]["prUrl"] = "https://github.com/acme/widgets/pull/7"
	f.WorkItems[item2]["status"] = "failed"
	f.WorkItems[item2]["lastError"] = "evaluation failed 3 times"
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "```json\n{\"items\": [{\"seq\": 3, \"title\": \"Make it pass, smaller\", \"brief\": \"Half.\", \"acceptance\": \"half passes\", \"depends_on\": [{\"seq\": 1, \"because\": \"uses the test item 1 merged\"}], \"repo\": \"acme/widgets\"}]}\n```"}
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(os.Stderr, "", 0)}
	if err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning", PausedReason: "item failed: evaluation failed 3 times"}, scopesOf(cfg)[0]); err != nil {
		t.Fatal(err)
	}
	p := h.jobs[0].Prompt
	for _, want := range []string{"The last plan blocked", "evaluation failed 3 times", "The items so far", "1. Write the failing test — merged (https://github.com/acme/widgets/pull/7)", "2. Make it pass — failed: evaluation failed 3 times", "Number your items from 3."} {
		if !strings.Contains(p, want) {
			t.Fatalf("re-plan prompt lacks %q:\n%s", want, p)
		}
	}
	// it checks main first: the goal's merged items with their PRs, the
	// search of recent merged PRs and commits, and what to do with work
	// main already has; the plan replaces only live items
	for _, want := range []string{
		"## What main already holds\n\nThese items of the goal merged; their work is on main:\n\n- 1. Write the failing test — https://github.com/acme/widgets/pull/7\n",
		"# Check main first",
		"search its recent merged pull requests and the commits on its default branch since the goal was set",
		"`gh pr list --repo <owner/name> --state merged --limit 50`",
		"the `mesh/ship-it-` branches",
		"`git log --oneline` in its checkout",
		"Leave out work main already has.",
		"say in its brief that an already_done check is expected",
		"finishes it already_done with the merged PRs or commits as evidence",
		"Your plan replaces only the live items; merged items stay.",
		"read-only, at their default branch, with its recent history",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("re-plan prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "- 2. Make it pass — https://") {
		t.Fatal("a failed item is not on main")
	}
	if len(f.CallsNamed("planGoal")) != 0 || len(f.CallsNamed("finishJob")) != 1 {
		t.Fatalf("the re-plan is handed in by the session and closed by finishJob: %+v", f.Calls)
	}
}

// adr-004: the node checks the planner's graph before PlanGoal.
const (
	repo2 = "cccccccc-2222-4222-8222-cccccccccccc"

	// update-nodes-from-console #6: a check on the item's own PR
	prCheckPlan = "```json\n{\"items\": [{\"seq\": 1, \"title\": \"Update from the console\", \"brief\": \"The update route.\", \"acceptance\": \"go test ./... passes, and the `deploy / verify` check concludes success on the item's pull request.\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"
	// update-nodes-from-console #4: another repo's file
	otherRepoPlan = "```json\n{\"items\": [{\"seq\": 1, \"title\": \"Console route\", \"brief\": \"The route.\", \"acceptance\": \"go test ./... passes; a specs/console-trd.md diff in acme/mesh-specs describes the route.\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"
	cleanPlan     = "```json\n{\"items\": [{\"seq\": 1, \"title\": \"Update from the console\", \"brief\": \"The update route.\", \"acceptance\": \"go test ./... passes and `grep -n Update internal/node/update.go` shows the handler.\", \"depends_on\": [], \"repo\": \"acme/widgets\"}]}\n```"
)

// twoOrgsServed is a node's served scopes of acme (sc) and beta, each
// with its connection.
func twoOrgsServed(sc Scope) func() Scopes {
	return func() Scopes { return connected(Scopes{sc, {Login: "beta", ID: orgB, NodeID: nodeB}}) }
}

// assertUnderOrg fails unless job's workspace, context dir and
// GH_CONFIG_DIR lie under root, its job's directory.
func assertUnderOrg(t *testing.T, job harness.Job, root string) {
	t.Helper()
	if job.Session == nil {
		t.Fatalf("%s: no pod on a sandboxed node", job.Label)
	}
	if !underDir(job.Workspace, root) {
		t.Fatalf("workspace %s not under %s", job.Workspace, root)
	}
	cx := filepath.Join(job.Workspace, ContextDir)
	if _, err := os.Stat(filepath.Join(cx, "INDEX.md")); err != nil || !underDir(cx, root) {
		t.Fatalf("context dir %s (under %s): %v", cx, root, err)
	}
	gh := envValue(job.Env, "GH_CONFIG_DIR")
	if gh == "" || !underDir(gh, root) {
		t.Fatalf("GH_CONFIG_DIR %q not under %s", gh, root)
	}
	if home := job.Session.Home; !underDir(home, root) {
		t.Fatalf("pod HOME %s not under %s", home, root)
	}
}

// On a sandboxed node serving two orgs a plan runs in its goal's org's
// pod: the job carries the pod, and its workspace, context dir and gh
// config dir lie in the goal's job (ADR-039).
func TestPlanOnASandboxedNodeRunsInItsOrgsPod(t *testing.T) {
	f := widgetsAlone(t)
	var logs bytes.Buffer
	pl, _, _, cfg := planTokenPlanner(f, t, &logs)
	h := &podGuardHarness{t: t, fakeHarness: fakeHarness{text: planTokenJSON}}
	sc := connected(scopesOf(cfg))[0]
	pl.Harness, pl.Sandbox, pl.Layout, pl.Served = h, nodeStoresSandbox(t), testLayout(t), twoOrgsServed(sc)
	if err := pl.Plan(context.Background(), shipIt(), sc); err != nil {
		t.Fatalf("plan: %v\n%s", err, logs.String())
	}
	if len(h.jobs) != 1 {
		t.Fatalf("jobs = %d", len(h.jobs))
	}
	j, _ := pl.Layout.Job(goalID)
	assertUnderOrg(t, h.jobs[0], j.Dir)
	if pods := pl.Sandbox.Runtime.(*sandbox.Fake).Pods; len(pods) != 1 {
		t.Fatalf("pods created = %d, want 1 before the harness", len(pods))
	}
}

// A sandboxed node whose pod does not start releases the plan claim
// with the reason, counts no try, holds the goal off the node, and never
// calls the harness — not on the host either.
func TestPlanPodStartFailureReleasesThePlanClaim(t *testing.T) {
	f := widgetsAlone(t)
	var logs bytes.Buffer
	pl, _, _, cfg := planTokenPlanner(f, t, &logs)
	h := &podGuardHarness{t: t, fakeHarness: fakeHarness{text: planTokenJSON}}
	sb := nodeStoresSandbox(t)
	sb.Runtime.(*sandbox.Fake).CreatePodErr = errors.New("pod create: no space left on device")
	sc := connected(scopesOf(cfg))[0]
	pl.Harness, pl.Sandbox, pl.Layout, pl.Served, pl.Declines = h, sb, testLayout(t), twoOrgsServed(sc), &Declines{}
	err := pl.Plan(context.Background(), shipIt(), sc)
	if _, ok := PodStartErrorOf(err); !ok {
		t.Fatalf("err = %v, want a PodStartError", err)
	}
	rel := f.CallsNamed("releasePlan")
	if len(rel) != 1 || rel[0].Input["aggregateId"] != goalID || !strings.Contains(rel[0].Input["note"].(string), "no space left on device") {
		t.Fatalf("releasePlan calls = %+v\n%s", rel, logs.String())
	}
	if len(h.jobs) != 0 || h.unsandboxed != 0 {
		t.Fatalf("harness called %d times (%d unsandboxed)", len(h.jobs), h.unsandboxed)
	}
	if pl.attempts[goalID] != 0 {
		t.Fatalf("attempts = %d, want 0", pl.attempts[goalID])
	}
	if !pl.Declines.Holds(goalID, nil, time.Now()) {
		t.Fatal("goal not held off this node")
	}
}

// A two-org node with no sandbox will not plan on the host: the claim
// is released sandbox_required and no session runs.
func TestPlanOnATwoOrgNodeWithSandboxOffIsReleased(t *testing.T) {
	f := widgetsAlone(t)
	var logs bytes.Buffer
	pl, _, h, cfg := planTokenPlanner(f, t, &logs)
	sc := connected(scopesOf(cfg))[0]
	pl.Layout, pl.Served, pl.Declines = testLayout(t), twoOrgsServed(sc), &Declines{}
	err := pl.Plan(context.Background(), shipIt(), sc)
	if !errors.Is(err, ErrHostServesSeveralOrgs) {
		t.Fatalf("err = %v", err)
	}
	rel := f.CallsNamed("releasePlan")
	if len(rel) != 1 || !strings.HasPrefix(rel[0].Input["note"].(string), SandboxRequiredPrefix) {
		t.Fatalf("releasePlan calls = %+v", rel)
	}
	if len(h.jobs) != 0 {
		t.Fatal("a plan ran on the host")
	}
}

// An unsandboxed node serving one org plans on the host as before: no
// pod, the gh config dir in the system temp dir.
func TestPlanOnAnUnsandboxedOneOrgNodeRunsOnTheHost(t *testing.T) {
	f := widgetsAlone(t)
	var logs bytes.Buffer
	pl, ws, h, cfg := planTokenPlanner(f, t, &logs)
	if err := pl.Plan(context.Background(), shipIt(), scopesOf(cfg)[0]); err != nil {
		t.Fatalf("plan: %v\n%s", err, logs.String())
	}
	if len(h.jobs) != 1 || h.jobs[0].Session != nil {
		t.Fatalf("jobs = %+v", h.jobs)
	}
	if h.jobs[0].Workspace != PlanDir(ws.root, "ship-it") {
		t.Fatalf("workspace %s", h.jobs[0].Workspace)
	}
	if gh := envValue(h.jobs[0].Env, "GH_CONFIG_DIR"); !underDir(gh, os.TempDir()) {
		t.Fatalf("GH_CONFIG_DIR %q", gh)
	}
}
