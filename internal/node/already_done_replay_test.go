package node

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// The replay of the stalled goal's item 7 (mesh ADR 003, Context): goal
// `stalled` planned item 3, whose work merged on main as a pull request;
// its replan then wrote item 7, a rebuild of item 3's work with the same
// acceptance, and the builder did it again from scratch at full price.
// Replayed on today's node, item 7's builder checks main first, finds
// item 3's merged pull request and hands the item back already done
// with it as evidence; the evaluator confirms the evidence on main; the
// platform marks the item merged. Nothing is pushed, no pull request is
// opened, no goal status table reaches git, and the build and the
// evaluation together cost under $1.

const (
	stalledGoal  = "dddddddd-5555-4555-8555-dddddddddddd"
	stalledItem3 = "eeeeeeee-5555-4555-8555-eeeeeeeeeeee"
	stalledItem7 = "eeeeeeee-6666-4666-8666-eeeeeeeeeeee"

	stalledAcceptance = "the goal page shows a stalled badge when no item moved for 2 hours; go test ./... passes"
	item3PR           = "https://github.com/acme/widgets/pull/3"
	item3Merge        = "3c0ffee5d1e8a7b6"

	// the cents of a short session, from the runner tests' fakes: the
	// build is fakeHarness's first step ($0.50, "wrote the test"), the
	// evaluation the Codex review of TestSecondOpinionBothPass ($0.40)
	lookAtMainUSD = 0.50
	confirmUSD    = 0.40
)

// fakeGitHub is the item's repo on GitHub as the sessions see it: the
// commits on main and the pull requests, merged or open. A session that
// pushes or opens a pull request would do it here.
type fakeGitHub struct {
	mu    sync.Mutex
	main  []string // SHAs on main, oldest first
	pulls []fakePull
}

type fakePull struct {
	URL, Branch, Body, MergeSHA string
	Merged                      bool
}

// mergedMatching is `gh pr list --state merged --search <words>`: the
// merged pull requests whose body carries the words.
func (g *fakeGitHub) mergedMatching(words string) []fakePull {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []fakePull
	for _, p := range g.pulls {
		if p.Merged && strings.Contains(p.Body, words) {
			out = append(out, p)
		}
	}
	return out
}

// onMain is `git merge-base --is-ancestor <sha> origin/main`.
func (g *fakeGitHub) onMain(sha string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.main {
		if s == sha {
			return true
		}
	}
	return false
}

func (g *fakeGitHub) merged(url string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.pulls {
		if p.URL == url {
			return p.Merged
		}
	}
	return false
}

// replaySessions is the harness: a builder that follows the prompt's
// check of main first, and an evaluator that checks the claim on main.
// Each keeps the INDEX.md it was given.
type replaySessions struct {
	gh    *fakeGitHub
	mu    sync.Mutex
	jobs  []harness.Job
	index map[harness.Kind]string
}

func (s *replaySessions) Name() string { return "claude-code" }

func (s *replaySessions) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	index, _ := os.ReadFile(filepath.Join(job.Workspace, ContextDir, IndexFile))
	s.mu.Lock()
	s.jobs = append(s.jobs, job)
	s.index[job.Kind] = string(index)
	s.mu.Unlock()
	if on == nil {
		on = func(harness.Progress) {}
	}
	on(harness.Progress{SessionID: "s-" + string(job.Kind)})
	switch job.Kind {
	case harness.KindBuild:
		// "Check main first": search the merged pull requests for the
		// acceptance; the prompt must ask for it and carry the words
		if !strings.Contains(job.Prompt, "mesh_finish outcome=already_done") || !strings.Contains(job.Prompt, stalledAcceptance) {
			return harness.Result{Success: true, Text: "Built it again.", CostUSD: 1.25}, nil
		}
		on(harness.Progress{Text: "gh pr list --state merged --search", CostUSD: 0.2})
		found := s.gh.mergedMatching(stalledAcceptance)
		if len(found) == 0 {
			return harness.Result{Success: true, Text: "Built it again.", CostUSD: 1.25}, nil
		}
		p := found[0]
		return harness.Result{Success: true, SessionID: "s-build", CostUSD: lookAtMainUSD,
			Text: "Checked main first: item 3's pull request merged the work.\n\n" + harness.AlreadyDoneLine + " " + p.URL + " (merged as " + p.MergeSHA + "): main's goal page shows the stalled badge and go test ./... passes there."}, nil
	case harness.KindEvaluate:
		// the claim: every cited pull request merged, every SHA on main
		verdict := "VERDICT: PASS"
		if !strings.Contains(job.Prompt, item3PR) || !s.gh.merged(item3PR) || !strings.Contains(job.Prompt, item3Merge) || !s.gh.onMain(item3Merge) {
			verdict = "VERDICT: FAIL"
		}
		text := "PR 3 is merged, " + item3Merge + " is on origin/main, and the acceptance holds there.\n\n" + verdict
		// the verdict goes to the node's mesh_result (ADR-041)
		handInScript(job, nil, text, nil)
		return harness.Result{Success: true, SessionID: "s-evaluate", CostUSD: confirmUSD, Text: text}, nil
	}
	return harness.Result{}, nil
}

// seedStalled is the goal as the replay finds it: item 3 merged with its
// pull request on main, item 7 ready with the same acceptance.
func seedStalled(t *testing.T) *gatewaytest.Fake {
	t.Helper()
	f := seed(t)
	delete(f.Goals, goalID)
	delete(f.WorkItems, item1)
	delete(f.WorkItems, item2)
	f.Goals[stalledGoal] = map[string]any{"id": stalledGoal, "orgId": orgID, "projectId": projID, "slug": "stalled",
		"title": "A goal can tell you the difference between working and stuck", "statement": "Show a stalled goal.",
		"acceptance": "go test ./... passes", "budgetCents": 2000, "spentCents": 900, "status": "running", "priority": "normal"}
	f.WorkItems[stalledItem3] = map[string]any{"id": stalledItem3, "goalId": stalledGoal, "orgId": orgID, "seq": 3,
		"title": "The goal page shows a stalled badge", "brief": "Badge a goal no item moved on.", "acceptance": stalledAcceptance,
		"repoId": repoID, "baseRef": "main", "status": "merged", "branch": "mesh/stalled-3", "prUrl": item3PR, "costCents": 310, "dependsOn": []string{}}
	f.WorkItems[stalledItem7] = map[string]any{"id": stalledItem7, "goalId": stalledGoal, "orgId": orgID, "seq": 7,
		"title": "Show a stalled badge on the goal page", "brief": "The replan's rebuild of item 3.", "acceptance": stalledAcceptance,
		"repoId": repoID, "baseRef": "main", "status": "ready", "dependsOn": []string{}}
	f.GitHubTokens[stalledItem7] = map[string]any{"token": "ghs_item7", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "repo": "acme/widgets"}
	return f
}

func TestReplayStalledItem7EndsAlreadyDoneUnderADollar(t *testing.T) {
	f := seedStalled(t)
	gh := &fakeGitHub{
		main:  []string{"1a2b3c4d5e6f7a8b", item3Merge, "9e8d7c6b5a4f3e2d"},
		pulls: []fakePull{{URL: item3PR, Branch: "mesh/stalled-3", Body: "Item 3.\n\nAcceptance: " + stalledAcceptance, MergeSHA: item3Merge, Merged: true}},
	}
	sessions := &replaySessions{gh: gh, index: map[harness.Kind]string{}}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	ctx := context.Background()
	if err := gw.ClaimWorkItem(ctx, stalledItem7, cfg.NodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	ws := &fakeWS{root: t.TempDir()}
	logger := log.New(io.Discard, "", 0)
	runner := &Runner{Credentials: testCreds(gw), GW: gw, Harness: sessions, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour, Log: logger}
	ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: sessions, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, Model: "sonnet", Log: logger}

	// the build: item 7 claimed and run
	item, err := gw.WorkItem(ctx, stalledItem7)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.RunItem(ctx, *item); err != nil {
		t.Fatal(err)
	}
	row := f.Item(stalledItem7)
	if row["status"] != "evaluating" || row["outcome"] != gateway.OutcomeAlreadyDone {
		t.Fatalf("after the build: %v", row)
	}
	evidence, _ := row["evidence"].(string)
	if !strings.Contains(evidence, item3PR) || !strings.Contains(evidence, item3Merge) {
		t.Fatalf("evidence = %q, want item 3's pull request and merge", evidence)
	}

	// the evaluation: the item as the platform holds it now. The reads
	// select outcome and evidence once schema.json carries them (ADR
	// 010); until then the row the builder handed in is set on the item
	// here, as evaluate_test's alreadyDoneItem1 does
	item, err = gw.WorkItem(ctx, stalledItem7)
	if err != nil {
		t.Fatal(err)
	}
	item.Outcome, item.Evidence = row["outcome"].(string), evidence
	if err := ev.Evaluate(ctx, *item); err != nil {
		t.Fatal(err)
	}

	// merged, with the outcome and no branch
	row = f.Item(stalledItem7)
	if row["status"] != "merged" || row["outcome"] != gateway.OutcomeAlreadyDone || row["branch"] != nil || row["prUrl"] != nil {
		t.Fatalf("final row: %v", row)
	}
	if n := len(f.CallsNamed("failWorkItem")) + len(f.CallsNamed("releaseWorkItem")); n != 0 {
		t.Fatalf("the item was failed or released %d times", n)
	}
	// zero pushes, zero pull requests
	if len(ws.pushed) != 0 || strings.Contains(strings.Join(ws.Events(), ","), "push") {
		t.Fatalf("pushed %v (events %v)", ws.pushed, ws.Events())
	}
	if n := len(f.CallsNamed("openPullRequest")); n != 0 || len(gh.pulls) != 1 {
		t.Fatalf("pull requests opened: %d on the platform, %d on GitHub", n, len(gh.pulls)-1)
	}
	// the build's checkout is a local branch it never pushes; the
	// evaluation's is main, detached
	if len(ws.prepared) != 2 || ws.prepared[1].Branch != "" || ws.prepared[1].BaseRef != "main" {
		t.Fatalf("the evaluation must check main out, not a branch: %+v", ws.prepared)
	}

	// the cost: every cent the node recorded, build and evaluation
	var recorded int
	for _, name := range []string{"reportWorkItemProgress", "finishWorkItem", "evaluateWorkItem"} {
		for _, c := range f.CallsNamed(name) {
			recorded += toCents(c.Input["costCents"])
		}
	}
	want := int(lookAtMainUSD*100) + int(confirmUSD*100)
	if recorded != want || toCents(row["costCents"]) != want || recorded >= 100 {
		t.Fatalf("recorded %d cents (row %v), want %d, under 100", recorded, row["costCents"], want)
	}

	// both sessions read item 3 as merged in INDEX.md
	item3Row := "| 3 | The goal page shows a stalled badge | merged | acme/widgets | `mesh/stalled-3` |\n"
	for _, kind := range []harness.Kind{harness.KindBuild, harness.KindEvaluate} {
		if index := sessions.index[kind]; !strings.Contains(index, "## Goal `stalled`") || !strings.Contains(index, item3Row) {
			t.Fatalf("%s session's INDEX.md lacks %q:\n%s", kind, item3Row, index)
		}
	}
	if !strings.Contains(sessions.index[harness.KindBuild], "| 7 | Show a stalled badge on the goal page | claimed | acme/widgets | — |\n") {
		t.Fatalf("the build's INDEX.md lacks item 7:\n%s", sessions.index[harness.KindBuild])
	}

	// no goal status table on any item branch: nothing committed, no
	// specs/goal-*.md anywhere in the node's workspaces
	assertNoGoalDoc(t, ws)
	filepath.WalkDir(ws.root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(filepath.Dir(path)) == "specs" && strings.HasPrefix(d.Name(), "goal-") {
			t.Fatalf("a goal status table was written: %s", path)
		}
		return nil
	})
	if len(sessions.jobs) != 2 {
		t.Fatalf("sessions: %d, want a build and an evaluation", len(sessions.jobs))
	}
}

// toCents reads a costCents the fake decoded from JSON or kept as int.
func toCents(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}
