package node

import (
	"context"
	"io"
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
		"{\"seq\": 2, \"title\": \"Make it pass\", \"brief\": \"Implement Thing.\", \"acceptance\": \"go test ./... passes\", \"depends_on\": [{\"seq\": 1, \"because\": \"uses TestThing item 1 adds\"}], \"repo\": \"widgets\", \"base_ref\": \"main\"}" +
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
	h := &fakeHarness{text: "```json\n{\"items\": [{\"seq\": 3, \"title\": \"Make it pass, smaller\", \"brief\": \"Half.\", \"acceptance\": \"half passes\", \"depends_on\": [{\"seq\": 1, \"because\": \"uses the test item 1 merged\"}], \"repo\": \"acme/widgets\"}]}\n```"}
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
	if err != nil || len(items) != 2 || items[1].DependsOn[0].Seq != 1 {
		t.Fatalf("good: %v %+v", err, items)
	}
	onMerged, err := harness.ParsePlan(`[{"seq":3,"title":"C","repo":"o/r","depends_on":[1]}]`, 1)
	if err != nil || len(onMerged) != 1 {
		t.Fatalf("on merged: %v", err)
	}
	if items[0].Model != "" || items[0].ModelReason != "" {
		t.Fatalf("no model: %+v", items[0])
	}
	withModel, err := harness.ParsePlan("```json\n[{\"seq\":1,\"title\":\"A\",\"repo\":\"o/r\",\"model\":\"haiku\",\"model_reason\":\"a rename\"}]\n```")
	if err != nil || withModel[0].Model != "haiku" || withModel[0].ModelReason != "a rename" {
		t.Fatalf("with model: %v %+v", err, withModel)
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

// Each item's model is parsed from the graph and handed in on its
// PlannedItem; an item without one, or with one no CLI knows, carries
// none. The planner itself runs on the strongest configured model.
func TestPlannerCarriesEachItemsModel(t *testing.T) {
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "```json\n{\"items\": [" +
		"{\"seq\": 1, \"title\": \"Docs\", \"acceptance\": \"README has a Models section\", \"repo\": \"acme/widgets\", \"model\": \" haiku \", \"model_reason\": \"docs-only\"}," +
		"{\"seq\": 2, \"title\": \"Scheduler\", \"acceptance\": \"go test ./... passes\", \"repo\": \"acme/widgets\", \"model\": \"claude-opus-5-5\", \"model_reason\": \"concurrency\"}," +
		"{\"seq\": 3, \"title\": \"Plain\", \"acceptance\": \"go test ./... passes\", \"repo\": \"acme/widgets\"}," +
		"{\"seq\": 4, \"title\": \"Odd\", \"acceptance\": \"go test ./... passes\", \"repo\": \"acme/widgets\", \"model\": \"gpt-5\"}" +
		"]}\n```"}
	var logged strings.Builder
	models := Models{Default: "sonnet", Build: "opus"}
	pl := &Planner{GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Model: models.PlanModel(), Log: log.New(&logged, "", 0)}
	if err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0]); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 || h.jobs[0].Model != "opus" {
		t.Fatalf("planner job model: %+v", h.jobs)
	}
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	items := calls[0].Input["items"].([]any)
	for i, want := range []any{"haiku", "claude-opus-5-5", nil, nil} {
		if got := items[i].(map[string]any)["model"]; got != want {
			t.Errorf("item %d model = %v, want %v", i+1, got, want)
		}
	}
	for _, want := range []string{
		`item 4 "Odd": model "gpt-5" is not haiku, sonnet, opus or a claude-* id; dropped`,
		`goal ship-it: item 1 "Docs": model haiku: docs-only`,
		`goal ship-it: item 2 "Scheduler": model claude-opus-5-5: concurrency`,
		"goal ship-it: item 3 \"Plain\": model none (the node's own)\n",
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logged.String())
		}
	}
}

// item 5: the scorecard's per-repo, per-model evidence the fake gateway
// serves rides the plan prompt as a table.
func TestPlannerReadsScorecardEvidence(t *testing.T) {
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.Scorecard[orgID] = map[string]any{"projects": []any{
		map[string]any{"projectId": projID, "windows": []any{
			map[string]any{"days": float64(7)},
			map[string]any{"days": float64(30), "models": []any{
				map[string]any{"model": "sonnet", "items": float64(14), "evaluated": float64(12), "firstPass": float64(4), "firstPassRate": 0.3, "costPerItemCents": float64(450)},
			}},
		}},
	}}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "```json\n{\"items\": [{\"seq\": 1, \"title\": \"Docs\", \"acceptance\": \"go test ./... passes\", \"repo\": \"acme/widgets\", \"model\": \"opus\"}]}\n```"}
	pl := &Planner{GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(os.Stderr, "", 0)}
	if err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0]); err != nil {
		t.Fatal(err)
	}
	if len(h.jobs) != 1 {
		t.Fatalf("planner sessions = %d, want 1", len(h.jobs))
	}
	for _, want := range []string{"acme/widgets", "sonnet", "0.30", "12", "$4.50", "under 0.5"} {
		if !strings.Contains(h.jobs[0].Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, h.jobs[0].Prompt)
		}
	}
}

// A platform without the scorecard query yields no evidence, and the
// plan still runs.
func TestPlannerWithoutScorecardOnAnOlderPlatform(t *testing.T) {
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	f.Unsupported["scorecard"] = true
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "```json\n{\"items\": [{\"seq\": 1, \"title\": \"Docs\", \"acceptance\": \"go test ./... passes\", \"repo\": \"acme/widgets\", \"model\": \"opus\"}]}\n```"}
	pl := &Planner{GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(os.Stderr, "", 0)}
	if err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0]); err != nil {
		t.Fatal(err)
	}
	if calls := f.CallsNamed("planGoal"); len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	if len(h.jobs) != 1 || strings.Contains(h.jobs[0].Prompt, "|") {
		t.Error("no scorecard: prompt still renders an evidence table")
	}
}

// evidenceFromScorecard on the platform's real shape (runsheet
// `platform/scorecard.Sums.Into`, `gateway.Client.Scorecard`'s
// returned project entry): `windows`, widest days first, each with its
// own `models` list — `model`, `evaluated`, `firstPassRate` (null when
// nothing was evaluated), `costPerItemCents`. The platform carries no
// repo breakdown, so a plain `models` row comes back with an empty
// repo; a null rate is skipped; the widest window (30 days) is read,
// not the narrower one (7 days) sitting beside it.
func TestEvidenceFromScorecardRealShape(t *testing.T) {
	sc := map[string]any{
		"projectId": projID, "projectName": "widgets",
		"windows": []any{
			map[string]any{"days": float64(7), "models": []any{
				map[string]any{"model": "sonnet", "items": float64(2), "evaluated": float64(2), "firstPass": float64(2), "firstPassRate": 1.0, "costPerItemCents": float64(999)},
			}},
			map[string]any{"days": float64(30), "models": []any{
				map[string]any{"model": "sonnet", "items": float64(14), "evaluated": float64(12), "firstPass": float64(4), "firstPassRate": 0.3, "costPerItemCents": float64(450)},
				map[string]any{"model": "opus", "items": float64(9), "evaluated": float64(9), "firstPass": float64(8), "firstPassRate": 8.0 / 9.0, "costPerItemCents": float64(1030)},
				map[string]any{"model": "haiku", "items": float64(1), "evaluated": float64(0), "firstPassRate": nil, "costPerItemCents": float64(50)},
			}},
		},
	}
	rows := evidenceFromScorecard(sc)
	byModel := map[string]harness.ModelEvidence{}
	for _, r := range rows {
		byModel[r.Model] = r
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (haiku's null rate skipped, the 7-day window unused)", rows)
	}
	sonnet, ok := byModel["sonnet"]
	if !ok {
		t.Fatalf("no sonnet row: %+v", rows)
	}
	if sonnet.Repo != "" {
		t.Errorf("sonnet.Repo = %q, want empty: the platform names no repo", sonnet.Repo)
	}
	if sonnet.Items != 12 {
		t.Errorf("sonnet.Items = %d, want 12 (evaluated, the rate's own denominator, not the 14 items built)", sonnet.Items)
	}
	if sonnet.FirstPass != 0.3 {
		t.Errorf("sonnet.FirstPass = %v, want 0.3 (the 30-day window, not the 7-day one's 1.0)", sonnet.FirstPass)
	}
	if sonnet.CostPerItemCents != 450 {
		t.Errorf("sonnet.CostPerItemCents = %d, want 450", sonnet.CostPerItemCents)
	}
	opus, ok := byModel["opus"]
	if !ok || opus.Items != 9 {
		t.Fatalf("opus row: %+v", opus)
	}
	if _, ok := byModel["haiku"]; ok {
		t.Errorf("haiku's null firstPassRate should be skipped, not zeroed: %+v", rows)
	}
}

// A `byRepo` breakdown some future scorecard may add is read repo by
// repo, at the project level or inside a window; the platform's own
// repo-less `models` rows come back beside them.
func TestEvidenceFromScorecardByRepo(t *testing.T) {
	sc := map[string]any{
		"byRepo": []any{
			map[string]any{"repo": "acme/widgets", "models": []any{
				map[string]any{"model": "sonnet", "evaluated": float64(6), "firstPassRate": 0.2, "costPerItemCents": float64(500)},
			}},
			map[string]any{"repo": "acme/console", "models": []any{
				map[string]any{"model": "sonnet", "evaluated": float64(6), "firstPassRate": 0.9, "costPerItemCents": float64(300)},
			}},
		},
	}
	rows := evidenceFromScorecard(sc)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	for _, r := range rows {
		if r.Repo == "" {
			t.Errorf("row %+v: byRepo entry lost its repo", r)
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
	for _, want := range []string{"## Your last answer was flagged", "- item 1 (Update from the console): its acceptance names a check on its own pull request", "this is the last ask"} {
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

// item 5: a model whose first-pass on a repo is under 0.5 over five or
// more items is flagged; a clean model, a different repo, and thin
// evidence (under five items) are not.
func TestModelAgainstEvidence(t *testing.T) {
	evidence := []harness.ModelEvidence{{Repo: "A", Model: "sonnet", FirstPass: 0.3, Items: 12, CostPerItemCents: 450}}
	specs := []harness.PlannedSpec{
		{Seq: 1, Title: "Sonnet on A", Repo: "A", Model: "sonnet"},
		{Seq: 2, Title: "Opus on A", Repo: "A", Model: "opus"},
		{Seq: 3, Title: "Sonnet on B", Repo: "B", Model: "sonnet"},
	}
	flags := modelAgainstEvidence(specs, evidence)
	if len(flags) != 1 || !strings.Contains(flags[0], "item 1 ") || !strings.Contains(flags[0], "model sonnet has first-pass 0.30 on A") {
		t.Fatalf("flags = %q", flags)
	}
	thin := []harness.ModelEvidence{{Repo: "A", Model: "sonnet", FirstPass: 0.3, Items: 3, CostPerItemCents: 450}}
	if flags := modelAgainstEvidence(specs[:1], thin); len(flags) != 0 {
		t.Errorf("under five items: flagged %q", flags)
	}
}

// trd §5 phase 2: a dependency without a because is dropped and
// logged; one with a because is kept, its reason on the PlannedItem
// (never sent to the platform) and in a line of the item's brief.
func TestPlannerDropsUnexplainedDependencies(t *testing.T) {
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "```json\n{\"items\": [" +
		`{"seq": 1, "title": "Foo type", "brief": "Add Foo.", "acceptance": "go test ./... passes", "repo": "acme/widgets"},` +
		`{"seq": 2, "title": "Same file", "brief": "Touch foo.go too.", "acceptance": "go test ./... passes", "repo": "acme/widgets"},` +
		`{"seq": 3, "title": "Use Foo", "brief": "Call Foo.", "acceptance": "go test ./... passes", "repo": "acme/widgets", "depends_on": [{"seq": 1, "because": "uses the Foo type item 1 adds"}, {"seq": 2, "because": "  "}]},` +
		`{"seq": 4, "title": "Docs", "brief": "Record it.", "acceptance": "README has a Foo section", "repo": "acme/widgets", "depends_on": [2]}` +
		"]}\n```"}
	var logged strings.Builder
	pl := &Planner{GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Log: log.New(&logged, "", 0)}
	goal := gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}
	if err := pl.Plan(context.Background(), goal, scopesOf(cfg)[0]); err != nil {
		t.Fatal(err)
	}
	calls := f.CallsNamed("planGoal")
	if len(calls) != 1 {
		t.Fatalf("plan calls: %+v", calls)
	}
	rows := calls[0].Input["items"].([]any)
	third, fourth := rows[2].(map[string]any), rows[3].(map[string]any)
	if deps := third["dependsOn"].([]any); len(deps) != 1 || deps[0] != float64(1) {
		t.Fatalf("item 3 dependsOn = %v, want [1]", deps)
	}
	if deps := fourth["dependsOn"].([]any); len(deps) != 0 {
		t.Fatalf("item 4 dependsOn = %v, want none", deps)
	}
	if third["brief"] != "Call Foo.\n\nDepends on #1 because uses the Foo type item 1 adds." || fourth["brief"] != "Record it." {
		t.Fatalf("briefs: %q, %q", third["brief"], fourth["brief"])
	}
	for _, row := range rows {
		for k := range row.(map[string]any) {
			if strings.Contains(strings.ToLower(k), "because") {
				t.Fatalf("the reason went to the platform: %v", row)
			}
		}
	}
	for _, want := range []string{
		"plan ship-it: item 3 dropped dependency on 2: no because\n",
		"plan ship-it: item 4 dropped dependency on 2: no because\n",
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logged.String())
		}
	}
	if strings.Contains(logged.String(), "dependency on 1") {
		t.Errorf("the explained dependency was dropped:\n%s", logged.String())
	}

	// the reason rides on the PlannedItem itself
	repos := map[string]gateway.Repo{"acme/widgets": {ID: repoID, Name: "widgets", DefaultBranch: "main"}}
	items, _, _, _, _, err := pl.ask(context.Background(), goal, "", t.TempDir(), 0, harness.PlanInput{}, repos, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := items[2]; len(got.DependsOn) != 1 || got.DependsOn[0] != 1 || len(got.DependsBecause) != 1 || got.DependsBecause[1] != "uses the Foo type item 1 adds" {
		t.Fatalf("item 3: %+v", got)
	}
	if got := items[3]; len(got.DependsOn) != 0 || got.DependsBecause != nil {
		t.Fatalf("item 4: %+v", got)
	}
}

// A plan handed in carries its session's usage report on planGoal: the
// credential the platform issued for the goal, the stream's tokens and
// cost, and the windows before and after.
func TestPlanReportsUsage(t *testing.T) {
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	issueCredential(f, goalID)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &streamHarness{file: usageStream(), text: cleanPlan}
	l := log.New(io.Discard, "", 0)
	pl := &Planner{GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Credentials: usageCredentials(gw, l), Log: l}
	if err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0]); err != nil {
		t.Fatal(err)
	}
	if reads := f.CallsNamed("planCredentials"); len(reads) != 1 {
		t.Fatalf("planCredentials: %+v", reads)
	}
	checkSessionUsage(t, f, "planGoal")
}

// A plan asked twice hands in one report for both sessions: their
// tokens and costs summed, as the plan's cost is.
func TestReplanReportsBothSessionsOnce(t *testing.T) {
	f := seed(t)
	f.Goals[goalID]["status"] = "planning"
	f.Projects[projID]["repoIds"] = []string{repoID}
	issueCredential(f, goalID)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1), OwnerID: f.UserID}
	ws := &fakeWS{root: t.TempDir()}
	h := &streamHarness{file: usageStream(), texts: []string{prCheckPlan, cleanPlan}}
	var logs strings.Builder
	l := log.New(&logs, "", 0)
	pl := &Planner{GW: gw, Harness: h, WS: ws, Cfg: cfg, WorkRoot: ws.root, Credentials: usageCredentials(gw, l), Log: l}
	if err := pl.Plan(context.Background(), gateway.Goal{ID: goalID, Slug: "ship-it", Title: "Ship it", ProjectID: projID, Status: "planning"}, scopesOf(cfg)[0]); err != nil {
		t.Fatal(err)
	}
	if len(h.Jobs()) != 2 {
		t.Fatalf("%d sessions", len(h.Jobs()))
	}
	if len(f.SessionUsage) != 1 || f.SessionUsage[0].Name != "planGoal" {
		t.Fatalf("usage: %+v", f.SessionUsage)
	}
	in := f.SessionUsage[0].Input
	if in["inputTokens"] != float64(2*60600) || in["listCostCents"] != float64(86) || in["credentialId"] != "cred-1" {
		t.Fatalf("usage: %v", in)
	}
	// one record, the carrier's, on the goal's label: the carried one is in it
	if recs := loggedUsage(t, logs.String()); len(recs) != 1 || recs[0].Label != "goal ship-it" || recs[0].Kind != "plan" || recs[0].Report.ListCostCents != 86 {
		t.Errorf("usage records = %+v:\n%s", recs, logs.String())
	}
}
