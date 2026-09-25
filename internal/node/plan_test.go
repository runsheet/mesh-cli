package node

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
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
		"{\"seq\": 2, \"title\": \"Make it pass\", \"brief\": \"Implement Thing.\", \"acceptance\": \"go test ./... passes\", \"depends_on\": [1], \"repo\": \"widgets\", \"base_ref\": \"main\"}" +
		"]}\n```"}
	pl := &Planner{GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, MaxBudgetUSD: 4, Log: log.New(os.Stderr, "", 0)}
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
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 || calls[0].Input["aggregateId"] != goal2 || calls[0].Input["by"] != f.UserID || calls[0].Input["costCents"] != float64(125) {
		t.Fatalf("plan calls: %+v", calls)
	}
	items := calls[0].Input["items"].([]any)
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if first["repoId"] != repoID || first["baseRef"] != "main" || first["title"] != "Write the failing test" || len(first["dependsOn"].([]any)) != 0 {
		t.Fatalf("item 1: %v", first)
	}
	if second["repoId"] != repoID || second["dependsOn"].([]any)[0] != float64(1) || second["acceptance"] != "go test ./... passes" {
		t.Fatalf("item 2: %v", second)
	}
	if f.Goals[goal2]["status"] != "awaiting_approval" || f.Goals[goal2]["spentCents"] != 125 {
		t.Fatalf("goal after plan: %v", f.Goals[goal2])
	}
	// the running goal was not planned; the session saw the repo and the context
	if len(h.jobs) != 1 || h.jobs[0].Kind != harness.KindPlan || h.jobs[0].MaxBudgetUSD != 4 || h.jobs[0].Workspace != PlanDir(ws.root, "new-thing") {
		t.Fatalf("job: %+v", h.jobs)
	}
	if len(ws.fetched) != 1 || !strings.HasPrefix(ws.fetched[0], "acme/widgets → "+filepath.Join(PlanDir(ws.root, "new-thing"), "repos", "widgets")) {
		t.Fatalf("fetched: %v", ws.fetched)
	}
	p := h.jobs[0].Prompt
	for _, want := range []string{"Build the new thing.", "the thing works", "$20.00", "`acme/widgets` in `repos/widgets/`", ContextDir, "adr-adr-001.md", "```json"} {
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
	if len(h.jobs) != 1 || len(f.CallsNamed("planGoal")) != 1 {
		t.Fatalf("a goal claimed elsewhere was planned: jobs=%d", len(h.jobs))
	}
	delete(f.Goals, goal3)

	// no graph: nothing handed in; after three tries the planner stops
	f.Goals[goal2]["status"] = "planning"
	h.text = "I could not decide."
	for i := 0; i < 5; i++ {
		loop.Tick(ctx)
		loop.wg.Wait()
	}
	if len(f.CallsNamed("planGoal")) != 1 || f.Goals[goal2]["status"] != "planning" {
		t.Fatalf("an unreadable plan must hand in nothing: %v", f.Goals[goal2]["status"])
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
	h := &fakeHarness{text: "```json\n{\"items\": [{\"seq\": 3, \"title\": \"Make it pass, smaller\", \"brief\": \"Half.\", \"acceptance\": \"half passes\", \"depends_on\": [1], \"repo\": \"acme/widgets\"}]}\n```"}
	pl := &Planner{GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(os.Stderr, "", 0)}
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
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	item := calls[0].Input["items"].([]any)[0].(map[string]any)
	if item["seq"] != float64(3) || item["dependsOn"].([]any)[0] != float64(1) {
		t.Fatalf("re-planned item: %v", item)
	}
}

func TestParsePlan(t *testing.T) {
	good := "notes\n```json\n{\"items\":[{\"seq\":1,\"title\":\"A\",\"repo\":\"o/r\"},{\"seq\":2,\"title\":\"B\",\"repo\":\"o/r\",\"depends_on\":[1]}]}\n```"
	items, err := harness.ParsePlan(good)
	if err != nil || len(items) != 2 || items[1].DependsOn[0] != 1 {
		t.Fatalf("good: %v %+v", err, items)
	}
	onMerged, err := harness.ParsePlan(`[{"seq":3,"title":"C","repo":"o/r","depends_on":[1]}]`, 1)
	if err != nil || len(onMerged) != 1 {
		t.Fatalf("on merged: %v", err)
	}
	bare, err := harness.ParsePlan(`[{"seq":1,"title":"A","repo":"o/r"}]`)
	if err != nil || len(bare) != 1 {
		t.Fatalf("bare: %v %+v", err, bare)
	}
	for name, bad := range map[string]string{
		"none":     "nothing here",
		"dup":      "```json\n[{\"seq\":1,\"title\":\"A\",\"repo\":\"o/r\"},{\"seq\":1,\"title\":\"B\",\"repo\":\"o/r\"}]\n```",
		"dangling": "```json\n[{\"seq\":1,\"title\":\"A\",\"repo\":\"o/r\",\"depends_on\":[3]}]\n```",
		"self":     "```json\n[{\"seq\":1,\"title\":\"A\",\"repo\":\"o/r\",\"depends_on\":[1]}]\n```",
		"no repo":  "```json\n[{\"seq\":1,\"title\":\"A\"}]\n```",
		"empty":    "```json\n{\"items\":[]}\n```",
	} {
		if _, err := harness.ParsePlan(bad); err == nil {
			t.Errorf("%s: parsed", name)
		}
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

// planOnce runs the planner once on the seeded goal with the harness's
// answers in turn, in a project of two repos (widgets and mesh-specs).
func planOnce(t *testing.T, answers ...string) (*gatewaytest.Fake, *fakeHarness, error) {
	t.Helper()
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Repos[repo2] = map[string]any{"id": repo2, "accountId": acctID, "name": "mesh-specs", "defaultBranch": "main"}
	f.Projects[projID]["repoIds"] = []string{repoID, repo2}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{texts: answers}
	pl := &Planner{GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(os.Stderr, "", 0)}
	err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0])
	return f, h, err
}

// A plan whose acceptance needs the `deploy / verify` check on the
// item's own PR is flagged; the planner is asked once more, told why,
// and only its clean second plan is handed in, with both sessions' cost.
func TestPlannerReasksOnAPRCheck(t *testing.T) {
	f, h, err := planOnce(t, prCheckPlan, cleanPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2", len(h.jobs))
	}
	if strings.Contains(h.jobs[0].Prompt, "Your last answer") {
		t.Fatal("the first ask flags nothing")
	}
	for _, want := range []string{"## Your last answer asked for what no evaluator can see", "- item 1 (Update from the console): its acceptance names a check on its own pull request", "this is the last ask"} {
		if !strings.Contains(h.jobs[1].Prompt, want) {
			t.Fatalf("re-ask lacks %q:\n%s", want, h.jobs[1].Prompt)
		}
	}
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 || calls[0].Input["costCents"] != float64(250) {
		t.Fatalf("plan calls: %+v", calls)
	}
	items := calls[0].Input["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["acceptance"] != "go test ./... passes and `grep -n Update internal/node/update.go` shows the handler." {
		t.Fatalf("handed in: %v", items)
	}
}

// Another repo's file in the acceptance is flagged too.
func TestPlannerReasksOnAnotherRepoFile(t *testing.T) {
	f, h, err := planOnce(t, otherRepoPlan, cleanPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 2 || !strings.Contains(h.jobs[1].Prompt, "a file in another repository, acme/mesh-specs") {
		t.Fatalf("sessions = %d; re-ask:\n%s", len(h.jobs), h.jobs[len(h.jobs)-1].Prompt)
	}
	if len(f.CallsNamed("planGoal")) != 1 {
		t.Fatal("the clean plan is handed in")
	}
}

// A plan still flagged after the re-ask is handed in as it stands, its
// flags logged; the node does not loop on the planner (adr-004).
func TestPlannerStillFlaggedHandsInTheSecond(t *testing.T) {
	f, h, err := planOnce(t, prCheckPlan, otherRepoPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 2 {
		t.Fatalf("planner sessions = %d, want 2 (one re-ask, no more)", len(h.jobs))
	}
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 || calls[0].Input["costCents"] != float64(250) {
		t.Fatalf("plan calls: %+v", calls)
	}
	items := calls[0].Input["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["title"] != "Console route" {
		t.Fatalf("handed in: %v, want the second plan as it stands", items)
	}
}

// A clean plan runs one session.
func TestPlannerCleanPlanAsksOnce(t *testing.T) {
	f, h, err := planOnce(t, cleanPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 || len(f.CallsNamed("planGoal")) != 1 || f.CallsNamed("planGoal")[0].Input["costCents"] != float64(125) {
		t.Fatalf("sessions = %d, plan calls = %d", len(h.jobs), len(f.CallsNamed("planGoal")))
	}
}

func TestUnverifiableAcceptance(t *testing.T) {
	widgets := gateway.Repo{ID: repoID, Name: "widgets", Owner: "acme", DefaultBranch: "main"}
	specs := gateway.Repo{ID: repo2, Name: "mesh-specs", Owner: "acme", DefaultBranch: "main"}
	project := map[string]gateway.Repo{"acme/widgets": widgets, "acme/mesh-specs": specs}
	check := func(acceptance string, in gateway.Repo) []string {
		return unverifiableAcceptance([]harness.PlannedSpec{{Seq: 4, Title: "T", Acceptance: acceptance}}, map[int]gateway.Repo{4: in}, project)
	}
	for _, text := range []string{prCheckPlan, otherRepoPlan} {
		sp, err := harness.ParsePlan(text)
		if err != nil {
			t.Fatal(err)
		}
		if flags := unverifiableAcceptance(sp, map[int]gateway.Repo{1: widgets}, project); len(flags) != 1 || !strings.HasPrefix(flags[0], "item 1 (") {
			t.Errorf("%q: flags %q", sp[0].Acceptance, flags)
		}
	}
	for name, acc := range map[string]string{
		"pr check":        "The `deploy / verify` check concludes success on the item's PR.",
		"ci":              "CI is green.",
		"on the pr":       "the lint job passes on the PR",
		"pr checks":       "every pull request check passes",
		"check name":      "`ci / test` succeeds",
		"deployed":        "the console is deployed and serves /nodes",
		"deploy":          "the deploy succeeds",
		"bare name":       "a specs/console-trd.md diff in mesh-specs",
		"name first":      "mesh-specs has specs/console-trd.md with a Nodes section",
		"owner/name/path": "acme/mesh-specs/specs/console-trd.md names the route",
		"checkout":        "repos/mesh-specs/specs/console-trd.md changes",
	} {
		if flags := check(acc, widgets); len(flags) == 0 {
			t.Errorf("%s: %q not flagged", name, acc)
		}
	}
	for name, acc := range map[string]string{
		"tests":         "go test ./... passes",
		"grep":          "`grep -n Rejected internal/harness/prompt.go` prints the field; go vet ./... is clean",
		"own repo":      "acme/widgets has internal/node/plan_check.go, and README.md says so",
		"own name":      "widgets' specs/adr-004.md exists",
		"own path":      "specs/console-trd.md has a Nodes section",
		"deploy code":   "go test ./internal/deploy/... passes and deploy.go reads the tag",
		"in the item's": "mesh-specs is named in no file; go test passes",
		"fixture words": "Tests in internal/node/plan_test.go show a pull request check fixture is flagged",
		"ci config":     "the CI workflow .github/workflows/ci.yml cancels older runs",
		"check in docs": "The check on the PR is described in README.md",
		"spec ref":      "per mesh-specs trd §3, internal/node/drain.go drains before exit",
	} {
		if flags := check(acc, widgets); len(flags) != 0 {
			t.Errorf("%s: %q flagged: %q", name, acc, flags)
		}
	}
	// the same path is the item's own when the item is in mesh-specs
	if flags := check("a specs/console-trd.md diff in mesh-specs", specs); len(flags) != 0 {
		t.Errorf("own repo flagged: %q", flags)
	}
}
