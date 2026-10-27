package node

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// An org links GitHub accounts beside its own (orgs-are-tenants): the
// runsheet org's loom project has its only repo, go-apis/loom, under
// the go-apis account. A project's repos are its repoIds, each owned by
// its own account's login — never the org's.
const (
	goApisAcct = "aaaaaaaa-2222-4222-8222-aaaaaaaaaaaa"
	loomRepo   = "cccccccc-3333-4333-8333-cccccccccccc"
)

// linkedAccount moves seed's project onto loom, a repo under a second
// account (go-apis) the org acme links, and points the items at it with
// no base ref of their own, so the repo's default branch is the base.
func linkedAccount(f *gatewaytest.Fake) {
	f.Accounts[goApisAcct] = map[string]any{"id": goApisAcct, "login": "go-apis"}
	f.Repos[loomRepo] = map[string]any{"id": loomRepo, "accountId": goApisAcct, "name": "loom", "defaultBranch": "trunk"}
	f.Projects[projID]["repoIds"] = []string{loomRepo}
	for _, id := range []string{item1, item2} {
		f.WorkItems[id]["repoId"], f.WorkItems[id]["baseRef"] = loomRepo, ""
		f.GitHubTokens[id]["repo"] = "go-apis/loom"
	}
}

func TestLoopPlansOnALinkedAccountRepo(t *testing.T) {
	f := seed(t)
	linkedAccount(f)
	const goal2 = "dddddddd-3333-4333-8333-dddddddddddd"
	f.Goals[goal2] = map[string]any{"id": goal2, "orgId": orgID, "projectId": projID, "slug": "new-thing", "title": "New thing",
		"statement": "Build the new thing.", "acceptance": "the thing works", "budgetCents": 2000, "spentCents": 0, "status": "planning"}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "```json\n{\"items\": [" +
		"{\"seq\": 1, \"title\": \"Make nested lists nullable\", \"brief\": \"In loom.\", \"acceptance\": \"go test ./... passes\", \"depends_on\": [], \"repo\": \"go-apis/loom\"}" +
		"]}\n```"}
	var logs bytes.Buffer
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 4, Log: log.New(&logs, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: pl.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Plan:     func(ctx context.Context, g gateway.Goal, sc Scope) { _ = pl.Plan(ctx, g, sc) }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()

	if strings.Contains(logs.String(), "has no repos enrolled") {
		t.Fatalf("a repo under a linked account is enrolled:\n%s", logs.String())
	}
	if len(ws.fetched) != 1 || !strings.HasPrefix(ws.fetched[0], "go-apis/loom → "+filepath.Join(PlanDir(ws.root, "new-thing"), "repos", "loom")) {
		t.Fatalf("fetched: %v", ws.fetched)
	}
	if len(h.jobs) != 1 || h.jobs[0].Kind != harness.KindPlan || !strings.Contains(h.jobs[0].Prompt, "`go-apis/loom` in `repos/loom/`") {
		t.Fatalf("the planner sees go-apis/loom: %+v", h.jobs)
	}
	if calls := f.CallsNamed("finishJob"); len(calls) != 1 {
		t.Fatalf("plan job closes: %+v\n%s", calls, logs.String())
	}
}

// The planner's repos map is keyed by the repo's own owner: a project
// with repos under both accounts has both, each by its own login.
func TestPlannerReposUnderBothAccountsLinkedAccount(t *testing.T) {
	f := seed(t)
	linkedAccount(f)
	f.Projects[projID]["repoIds"] = []string{repoID, loomRepo}
	const goal2 = "dddddddd-3333-4333-8333-dddddddddddd"
	f.Goals[goal2] = map[string]any{"id": goal2, "orgId": orgID, "projectId": projID, "slug": "new-thing", "status": "planning"}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, NodeID: "22222222-2222-4222-8222-222222222222"}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "no graph"}
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 4, Log: log.New(&bytes.Buffer{}, "", 0)}
	goal, err := gw.Goal(context.Background(), goal2)
	if err != nil {
		t.Fatal(err)
	}
	_, repos, err := pl.projectRepoMap(context.Background(), *goal, scopesOf(cfg)[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || repos["acme/widgets"].ID != repoID || repos["go-apis/loom"].ID != loomRepo {
		t.Fatalf("repos: %v", repos)
	}
	_ = pl.Plan(context.Background(), *goal, scopesOf(cfg)[0])
	got := strings.Join(ws.fetched, "\n")
	for _, want := range []string{"acme/widgets → ", "go-apis/loom → "} {
		if !strings.Contains(got, want) {
			t.Fatalf("fetched lacks %q: %v", want, ws.fetched)
		}
	}
}

// A project with no repos at all still says so.
func TestPlannerNoReposLinkedAccount(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	pl := &Planner{Credentials: testCreds(gw), GW: gw, Log: log.New(&bytes.Buffer{}, "", 0)}
	_, _, err := pl.projectRepoMap(context.Background(), gateway.Goal{ProjectID: projID}, Scope{Login: "acme", ID: orgID, AccountID: acctID})
	if err == nil || !strings.Contains(err.Error(), "project mesh has no repos enrolled") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoopResearchesALinkedAccountRepo(t *testing.T) {
	f := seed(t)
	linkedAccount(f)
	direction(f, dirID, projID, "overnight", "Loom reads every schema.", 3)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "```json\n{\"goals\": []}\n```"}
	var logs bytes.Buffer
	dir := &Director{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 4, Log: log.New(&logs, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: dir.Log,
		Dispatch: func(context.Context, gateway.WorkItem) {},
		Research: func(ctx context.Context, dn gateway.Direction, p gateway.Project, sc Scope) {
			if err := dir.Research(ctx, dn, p, sc); err != nil {
				t.Errorf("research: %v", err)
			}
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	loop.Tick(ctx)
	loop.wg.Wait()

	if len(ws.fetched) != 1 || !strings.HasPrefix(ws.fetched[0], "go-apis/loom → "+filepath.Join(ResearchDir(ws.root, "mesh", "overnight"), "repos", "loom")) {
		t.Fatalf("fetched: %v\n%s", ws.fetched, logs.String())
	}
	if len(h.jobs) != 1 || !strings.Contains(h.jobs[0].Prompt, "`go-apis/loom` in `repos/loom/`") {
		t.Fatalf("the director sees go-apis/loom: %+v", h.jobs)
	}
	if len(f.CallsNamed("recordDirectionResearch")) != 1 {
		t.Fatalf("no hand-in:\n%s", logs.String())
	}
}

// A direction on a project with repos under both accounts fetches both.
func TestResearchReposUnderBothAccountsLinkedAccount(t *testing.T) {
	f := seed(t)
	linkedAccount(f)
	f.Projects[projID]["repoIds"] = []string{repoID, loomRepo}
	direction(f, dirID, projID, "overnight", "Loom reads every schema.", 3)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, NodeID: "22222222-2222-4222-8222-222222222222", OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "```json\n{\"goals\": []}\n```"}
	dir := &Director{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 4, Log: log.New(&bytes.Buffer{}, "", 0)}
	ctx := context.Background()
	project, err := gw.Project(ctx, projID)
	if err != nil {
		t.Fatal(err)
	}
	directions, err := gw.Directions(ctx, orgID, projID)
	if err != nil || len(directions) != 1 {
		t.Fatalf("directions: %v %v", directions, err)
	}
	if err := gw.ClaimDirectionResearch(ctx, dirID, cfg.NodeID); err != nil {
		t.Fatal(err)
	}
	if err := dir.Research(ctx, directions[0], *project, scopesOf(cfg)[0]); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(ws.fetched, "\n")
	for _, want := range []string{"acme/widgets → ", "go-apis/loom → "} {
		if !strings.Contains(got, want) {
			t.Fatalf("fetched lacks %q: %v", want, ws.fetched)
		}
	}
}

func TestLoopBuildsOnALinkedAccountRepo(t *testing.T) {
	f := seed(t)
	linkedAccount(f)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node", Harnesses: []string{"claude-code"}, Capacity: 1, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{}
	runner := &Runner{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, MaxBudgetUSD: 5, ProgressEvery: time.Hour, Log: log.New(&bytes.Buffer{}, "", 0)}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: runner.Log,
		Dispatch: func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	waitFor(t, func() bool { return f.Item(item1)["status"] == "evaluating" })

	ws.mu.Lock()
	defer ws.mu.Unlock()
	if len(ws.prepared) != 1 || ws.prepared[0].Repo != "go-apis/loom" || ws.prepared[0].BaseRef != "trunk" {
		t.Fatalf("prepared: %+v", ws.prepared)
	}
	if len(h.jobs) != 1 || !strings.Contains(h.jobs[0].Prompt, "go-apis/loom") {
		t.Fatalf("the builder works in go-apis/loom: %+v", h.jobs)
	}
}
