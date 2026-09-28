package node

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
)

// tokenGW hands out a sequence of GitHub tokens, the last one repeated,
// or answers as an old platform when unsupported.
type tokenGW struct {
	Gateway
	mu          sync.Mutex
	tokens      []*gateway.GitHubToken
	issued      int
	unsupported bool
}

func (g *tokenGW) IssueItemRepoToken(_ context.Context, itemID, nodeID string) (*gateway.GitHubToken, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unsupported {
		return nil, &gateway.Error{Message: `Cannot query field "issueItemRepoToken" on type "Mutation".`, Unsupported: true}
	}
	i := g.issued
	if i >= len(g.tokens) {
		i = len(g.tokens) - 1
	}
	g.issued++
	return g.tokens[i], nil
}

func (g *tokenGW) Issued() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.issued
}

// runnerOn is a runner on the seeded fake with item1 claimed, and the
// item to run.
func runnerOn(t *testing.T, h harness.Harness, wrap func(Gateway) Gateway) (*Runner, *fakeWS, gateway.WorkItem, func(string) map[string]any) {
	t.Helper()
	return runnerOnFake(t, seed(t), h, wrap)
}

// runnerOnFake is runnerOn against a fake the test has set up.
func runnerOnFake(t *testing.T, f *gatewaytest.Fake, h harness.Harness, wrap func(Gateway) Gateway) (*Runner, *fakeWS, gateway.WorkItem, func(string) map[string]any) {
	t.Helper()
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	if err := gw.ClaimWorkItem(context.Background(), item1, cfg.NodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	var g Gateway = gw
	if wrap != nil {
		g = wrap(gw)
	}
	ws := &fakeWS{root: t.TempDir()}
	r := &Runner{GW: g, Harness: h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour, Log: log.New(os.Stderr, "", 0)}
	var item gateway.WorkItem
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Title = item1, orgID, goalID, repoID, 1, "Write the failing test"
	return r, ws, item, f.Item
}

func envOf(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// hasGHEnv says env sets gh's own variables: a fallback session keeps
// the operator's.
func hasGHEnv(env []string) bool {
	_, tok := envOf(env, "GH_TOKEN")
	_, dir := envOf(env, "GH_CONFIG_DIR")
	return tok || dir
}

// A build session carries the item's token as GH_TOKEN and a fresh,
// empty GH_CONFIG_DIR — not the operator's ~/.config/gh — and the
// workspace is authorized with the token before it is prepared.
func TestBuildSessionRunsOnTheItemsToken(t *testing.T) {
	var env []string
	var ghDirEntries []os.DirEntry
	var ghDirErr error
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		env = job.Env
		if dir, ok := envOf(job.Env, "GH_CONFIG_DIR"); ok {
			ghDirEntries, ghDirErr = os.ReadDir(dir)
		}
		return harness.Result{Success: true, Text: "done"}, nil
	})
	r, ws, item, row := runnerOn(t, h, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if row(item1)["status"] != "evaluating" {
		t.Fatalf("row: %v", row(item1))
	}
	if tok, _ := envOf(env, "GH_TOKEN"); tok != "ghs_item1" {
		t.Fatalf("GH_TOKEN = %q, env %v", tok, env)
	}
	ghDir, ok := envOf(env, "GH_CONFIG_DIR")
	if !ok || ghDir == "" {
		t.Fatalf("no GH_CONFIG_DIR: %v", env)
	}
	if ghDirErr != nil || len(ghDirEntries) != 0 {
		t.Fatalf("GH_CONFIG_DIR must be an empty directory: %v %v", ghDirErr, ghDirEntries)
	}
	if cfgDir, err := os.UserConfigDir(); err == nil {
		operator := filepath.Join(cfgDir, "gh")
		if rel, err := filepath.Rel(operator, ghDir); err == nil && !strings.HasPrefix(rel, "..") {
			t.Fatalf("GH_CONFIG_DIR %s is inside the operator's %s", ghDir, operator)
		}
	}
	if strings.HasPrefix(ghDir, ws.root) {
		t.Fatalf("GH_CONFIG_DIR %s must not be in the workspace", ghDir)
	}
	if _, err := os.Stat(ghDir); !os.IsNotExist(err) {
		t.Fatalf("GH_CONFIG_DIR %s outlived its session: %v", ghDir, err)
	}
	if got := strings.Join(ws.Events(), ","); got != "authorize ghs_item1,prepare,push" {
		t.Fatalf("workspace events = %s", got)
	}
}

// A token near its expiry is renewed, and the workspace re-authorized
// with the new one, before the push.
func TestRunnerRefreshesATokenNearExpiry(t *testing.T) {
	tg := &tokenGW{tokens: []*gateway.GitHubToken{
		{Token: "ghs_old", ExpiresAt: time.Now().Add(time.Minute), Repository: "acme/widgets"},
		{Token: "ghs_new", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets"},
	}}
	var env []string
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		env = job.Env
		return harness.Result{Success: true, Text: "done"}, nil
	})
	r, ws, item, _ := runnerOn(t, h, func(g Gateway) Gateway { tg.Gateway = g; return tg })
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if tok, _ := envOf(env, "GH_TOKEN"); tok != "ghs_old" {
		t.Fatalf("the session starts on the token it was given: %q", tok)
	}
	if got := strings.Join(ws.Events(), ","); got != "authorize ghs_old,prepare,authorize ghs_new,push" {
		t.Fatalf("workspace events = %s", got)
	}
	if tg.Issued() != 2 {
		t.Fatalf("issued %d tokens, want 2 (the fresh one is not renewed)", tg.Issued())
	}
}

// The progress ticker renews a token that nears expiry mid-session, so a
// session that outlives the hour keeps its credential.
func TestRunnerRefreshesOnTheProgressTicker(t *testing.T) {
	tg := &tokenGW{tokens: []*gateway.GitHubToken{
		{Token: "ghs_old", ExpiresAt: time.Now().Add(time.Minute), Repository: "acme/widgets"},
		{Token: "ghs_new", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets"},
	}}
	var ws *fakeWS
	var during []string
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		waitFor(t, func() bool { return tg.Issued() == 2 && len(ws.Events()) == 3 })
		during = ws.Events()
		return harness.Result{Success: true, Text: "done"}, nil
	})
	r, w, item, _ := runnerOn(t, h, func(g Gateway) Gateway { tg.Gateway = g; return tg })
	ws = w
	r.ProgressEvery = 5 * time.Millisecond
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(during, ","); got != "authorize ghs_old,prepare,authorize ghs_new" {
		t.Fatalf("events during the session = %s", got)
	}
}

// An old platform without the mutation leaves everything as it was: no
// token in the session, the workspace on the operator's credentials.
func TestRunnerFallsBackOnAnOldPlatform(t *testing.T) {
	tg := &tokenGW{unsupported: true}
	h := &fakeHarness{}
	r, ws, item, row := runnerOn(t, h, func(g Gateway) Gateway { tg.Gateway = g; return tg })
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if row(item1)["status"] != "evaluating" {
		t.Fatalf("row: %v", row(item1))
	}
	if len(h.jobs) != 1 || hasGHEnv(h.jobs[0].Env) {
		t.Fatalf("the fallback session's env must be the operator's: %+v", h.jobs)
	}
	if got := strings.Join(ws.Events(), ","); got != "prepare,push" {
		t.Fatalf("workspace events = %s", got)
	}
}

// A platform that refuses the item a token fails it: it is not run on
// the operator's login instead.
func TestRunnerFailsWhenTheTokenIsRefused(t *testing.T) {
	h := &fakeHarness{}
	// the platform refuses a token to any node but the claim's, judging
	// by the caller's credential; the gateway client is wrapped so its
	// token call answers that refusal
	r, ws, item, row := runnerOn(t, h, func(g Gateway) Gateway { return refusingTokenGW{g} })
	err := r.RunItem(context.Background(), item)
	var ge *gateway.Error
	if err == nil || !errors.As(err, &ge) {
		t.Fatalf("err = %v", err)
	}
	if row(item1)["status"] != "failed" || len(h.jobs) != 0 || len(ws.Events()) != 0 {
		t.Fatalf("a refused item must not run: %v jobs=%d events=%v", row(item1), len(h.jobs), ws.Events())
	}
}

// The evaluator authorizes the workspace with the item's token before it
// prepares the branch and reads its diff, and its session carries it.
func TestEvaluatorRunsOnTheItemsToken(t *testing.T) {
	h := &fakeHarness{text: "Fine.\nVERDICT: PASS"}
	r, ws, item, row := runnerOn(t, h, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	ws.mu.Lock()
	ws.events = nil
	ws.mu.Unlock()
	ev := &Evaluator{GW: r.GW, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, Log: r.Log}
	item.Branch = "mesh/ship-it-1"
	if err := ev.Evaluate(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ws.Events(), ","); got != "authorize ghs_item1,prepare,diff" {
		t.Fatalf("workspace events = %s", got)
	}
	if len(h.jobs) != 2 || h.jobs[1].Kind != harness.KindEvaluate {
		t.Fatalf("jobs: %+v", h.jobs)
	}
	if tok, _ := envOf(h.jobs[1].Env, "GH_TOKEN"); tok != "ghs_item1" {
		t.Fatalf("evaluator GH_TOKEN = %q", tok)
	}
	if _, ok := envOf(h.jobs[1].Env, "GH_CONFIG_DIR"); !ok {
		t.Fatalf("evaluator has no GH_CONFIG_DIR: %v", h.jobs[1].Env)
	}
	if row(item1)["status"] == "evaluating" {
		t.Fatalf("no verdict recorded: %v", row(item1))
	}
}

func TestGitHubTokenNeedsRefresh(t *testing.T) {
	for _, c := range []struct {
		tok  *gateway.GitHubToken
		want bool
	}{
		{nil, true},
		{&gateway.GitHubToken{}, true},
		{&gateway.GitHubToken{Token: "t"}, false},
		{&gateway.GitHubToken{Token: "t", ExpiresAt: time.Now().Add(time.Hour)}, false},
		{&gateway.GitHubToken{Token: "t", ExpiresAt: time.Now().Add(time.Minute)}, true},
		{&gateway.GitHubToken{Token: "t", ExpiresAt: time.Now().Add(-time.Minute)}, true},
	} {
		if got := GitHubTokenNeedsRefresh(c.tok); got != c.want {
			t.Errorf("%+v: %v, want %v", c.tok, got, c.want)
		}
	}
}

// refusingTokenGW is a gateway whose token call is refused the way the
// platform refuses a node that does not hold the item's claim.
type refusingTokenGW struct{ Gateway }

func (refusingTokenGW) IssueItemRepoToken(ctx context.Context, itemID, nodeID string) (*gateway.GitHubToken, error) {
	return nil, &gateway.Error{Message: "issueItemRepoToken: the item is not this node's claim"}
}

// syncBuf is a log sink several sessions write at once.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A node claiming a batch of items against a platform without the
// mutation says so once for the run — its builds and its evaluations
// share the notice — and every item still runs on the operator's own
// credentials to its end.
func TestOldPlatformFallbackIsLoggedOnceAcrossARun(t *testing.T) {
	f := seed(t)
	f.Unsupported["issueItemRepoToken"] = true
	f.WorkItems[item2]["status"] = "ready"
	f.WorkItems[item2]["dependsOn"] = []string{}
	items := []string{item1, item2}
	for i, id := range []string{"99999999-1111-4111-8111-999999999991", "99999999-1111-4111-8111-999999999992"} {
		f.WorkItems[id] = map[string]any{"id": id, "goalId": goalID, "orgId": orgID, "seq": 3 + i, "title": "More",
			"brief": "More.", "acceptance": "go test passes", "repoId": repoID, "baseRef": "main", "status": "ready", "dependsOn": []string{}}
		items = append(items, id)
	}
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(len(items))}
	if err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node", Harnesses: []string{"claude-code"}, Capacity: cfg.Capacity.Max, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	var out syncBuf
	logger := log.New(&out, "", 0)
	fallback := &TokenFallback{}
	ws := &fakeWS{root: t.TempDir()}
	h := &fakeHarness{text: "Fine.\nVERDICT: PASS"}
	runner := &Runner{GW: gw, Harness: h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour, Fallback: fallback, Log: logger}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: logger,
		Dispatch: func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) }}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loop.Tick(ctx)
	waitFor(t, func() bool {
		for _, id := range items {
			if f.Item(id)["status"] != "evaluating" {
				return false
			}
		}
		return true
	})
	for _, id := range items {
		row := f.Item(id)
		ev := &Evaluator{GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, Fallback: fallback, Log: logger}
		it := gateway.WorkItem{ID: id, OrgID: orgID, GoalID: goalID, RepoID: repoID, Seq: row["seq"].(int), Title: row["title"].(string), Branch: row["branch"].(string)}
		if err := ev.Evaluate(ctx, it); err != nil {
			t.Fatal(err)
		}
		if f.Item(id)["status"] != "review" {
			t.Fatalf("item %s: %v", id, f.Item(id))
		}
	}
	if n := len(f.CallsNamed("failWorkItem")); n != 0 {
		t.Fatalf("%d items failed on the fallback", n)
	}
	if got := strings.Count(out.String(), "the platform issues no item GitHub tokens"); got != 1 {
		t.Fatalf("fallback said %d times, want once:\n%s", got, out.String())
	}
	for _, e := range ws.Events() {
		if strings.HasPrefix(e, "authorize") {
			t.Fatalf("a fallback workspace must stay on the operator's credentials: %v", ws.Events())
		}
	}
	for _, j := range h.jobs {
		if hasGHEnv(j.Env) {
			t.Fatalf("a fallback session's env must be the operator's: %v", j.Env)
		}
	}
	if len(ws.pushed) != len(items) || len(h.jobs) != 2*len(items) {
		t.Fatalf("pushed %v, %d sessions", ws.pushed, len(h.jobs))
	}
}

// modelGW answers AgentModel from a table and records the progress notes.
type modelGW struct {
	Gateway
	mu     sync.Mutex
	agents map[string]string
	notes  []string
}

func (g *modelGW) AgentModel(_ context.Context, agentID string) (string, error) {
	return g.agents[agentID], nil
}

func (g *modelGW) ReportWorkItemProgress(ctx context.Context, itemID, note string, costCents int, sessionID string) error {
	g.mu.Lock()
	g.notes = append(g.notes, note)
	g.mu.Unlock()
	return g.Gateway.ReportWorkItemProgress(ctx, itemID, note, costCents, sessionID)
}

// The build runs on the item's model, then its agent's, then the node's
// own; the evaluator on the strongest of the node's, the builder's and
// sonnet; a rebase on the builder's choice — and an item that names
// neither (an older platform) runs all three exactly as before.
func TestSessionsRunOnTheItemsModel(t *testing.T) {
	agent := gateway.AgentID(mustUUID(orgID), uuid.Nil, "fast-builder").String()
	for _, c := range []struct {
		name                         string
		model, agentSlug             string
		agentModel                   string
		nodeBuild, nodeEval          string
		wantBuild, wantEval, wantLog string
	}{
		{name: "item haiku", model: "haiku", nodeBuild: "opus", wantBuild: "haiku", wantEval: "sonnet"},
		{name: "older gateway", nodeBuild: "sonnet", nodeEval: "haiku", wantBuild: "sonnet", wantEval: "haiku"},
		{name: "older gateway, CLI default", wantBuild: "", wantEval: "", wantLog: "the CLI's default model"},
		{name: "agent only", agentSlug: "fast-builder", agentModel: "opus", nodeBuild: "haiku", wantBuild: "opus", wantEval: "opus"},
		{name: "item beats agent", model: "haiku", agentSlug: "fast-builder", agentModel: "opus", wantBuild: "haiku", wantEval: "sonnet"},
		{name: "agent without a model", agentSlug: "fast-builder", nodeBuild: "sonnet", nodeEval: "haiku", wantBuild: "sonnet", wantEval: "haiku"},
		{name: "item opus", model: "opus", nodeEval: "sonnet", wantBuild: "opus", wantEval: "opus"},
		{name: "node evaluate stronger", model: "haiku", nodeEval: "claude-opus-5-5", wantBuild: "haiku", wantEval: "claude-opus-5-5"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := &fakeHarness{text: "Fine.\nVERDICT: PASS"}
			mg := &modelGW{agents: map[string]string{}}
			if c.agentModel != "" {
				mg.agents[agent] = c.agentModel
			}
			f := seed(t)
			r, ws, item, row := runnerOnFake(t, f, h, func(g Gateway) Gateway { mg.Gateway = g; return mg })
			var logs strings.Builder
			r.Log = log.New(&logs, "", 0)
			r.Model = c.nodeBuild
			item.Model, item.AgentSlug = c.model, c.agentSlug
			if err := r.RunItem(context.Background(), item); err != nil {
				t.Fatal(err)
			}
			ev := &Evaluator{GW: mg, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, Model: c.nodeEval, Log: r.Log}
			item.Branch = "mesh/ship-it-1"
			if err := ev.Evaluate(context.Background(), item); err != nil {
				t.Fatal(err)
			}
			if len(h.jobs) != 2 || h.jobs[0].Kind != harness.KindBuild || h.jobs[1].Kind != harness.KindEvaluate {
				t.Fatalf("jobs: %+v", h.jobs)
			}
			if h.jobs[0].Model != c.wantBuild {
				t.Fatalf("build Model = %q, want %q", h.jobs[0].Model, c.wantBuild)
			}
			if h.jobs[1].Model != c.wantEval {
				t.Fatalf("evaluate Model = %q, want %q", h.jobs[1].Model, c.wantEval)
			}
			build, eval := c.wantBuild, c.wantEval
			if c.wantLog != "" {
				build, eval = c.wantLog, c.wantLog
			}
			for _, want := range []string{"item ship-it/1: build on " + build + "\n", "item ship-it/1: evaluate on " + eval + ": mesh/ship-it-1"} {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("node.log lacks %q:\n%s", want, logs.String())
				}
			}
			if len(mg.notes) == 0 || mg.notes[0] != "session started: mesh/ship-it-1 on "+build {
				t.Fatalf("first progress note: %q", mg.notes)
			}
			if got := row(item1)["evaluation"]; got != "Fine.\n\n(evaluated on "+eval+")" {
				t.Fatalf("evaluation = %q", got)
			}

			// a rebase works on the builder's branch, so on its model
			f.RebaseTokens = true
			f.WorkItems[item1]["status"], f.WorkItems[item1]["rebaseState"] = "review", "claimed"
			rb := &Rebaser{GW: mg, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, Model: c.nodeBuild, ProgressEvery: time.Hour, Log: r.Log}
			_ = rb.Rebase(context.Background(), item) // the job is what is checked
			if len(h.jobs) != 3 || h.jobs[2].Kind != harness.KindRebase || h.jobs[2].Model != c.wantBuild {
				t.Fatalf("rebase job: %+v\n%s", h.jobs[len(h.jobs)-1], logs.String())
			}
			if want := "item ship-it/1: rebase on " + build + ": mesh/ship-it-1 onto main"; !strings.Contains(logs.String(), want) {
				t.Fatalf("node.log lacks %q:\n%s", want, logs.String())
			}
		})
	}
}

// A build session that finds the item's work on main hands it back
// already done (adr-003): the node pushes nothing, fails nothing, and
// finishes the item once with the evidence and no branch — on to its
// evaluation, with no pull request to open.
func TestBuildFinishesAlreadyDoneWithoutAPush(t *testing.T) {
	ev := "https://github.com/acme/widgets/pull/3 (abc1234): main's TestThing already covers the acceptance."
	h := &fakeHarness{text: "Checked main first: the work merged with item 3.\n\nALREADY DONE: " + ev}
	f := seed(t)
	r, ws, item, row := runnerOnFake(t, f, h, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(ws.pushed) != 0 {
		t.Fatalf("an already_done finish pushed %v", ws.pushed)
	}
	if got := strings.Join(ws.Events(), ","); strings.Contains(got, "push") {
		t.Fatalf("workspace events = %s", got)
	}
	finishes := f.CallsNamed("finishWorkItem")
	if len(finishes) != 1 {
		t.Fatalf("finishWorkItem calls = %d, want 1", len(finishes))
	}
	in := finishes[0].Input
	if in["outcome"] != "already_done" || in["evidence"] != ev || in["branch"] != nil || in["headSha"] != nil {
		t.Fatalf("finish = %v", in)
	}
	if n := len(f.CallsNamed("failWorkItem")); n != 0 {
		t.Fatalf("failWorkItem called %d times", n)
	}
	if n := len(f.CallsNamed("openPullRequest")); n != 0 {
		t.Fatalf("openPullRequest called %d times", n)
	}
	if got := row(item1); got["status"] != "evaluating" || got["outcome"] != "already_done" || got["branch"] != nil {
		t.Fatalf("row: %v", got)
	}
	if len(h.jobs) != 1 || !strings.Contains(h.jobs[0].Prompt, "mesh_finish outcome=already_done") {
		t.Fatal("the build session's prompt must carry the check of main first")
	}
}
