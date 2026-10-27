package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
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
	// a session runs only on what the claim-time read issues: the fake
	// issues the item one Claude subscription unless the test set its own
	issueClaude(f, item1)
	r := &Runner{Credentials: testCreds(g), GW: g, Harness: h, WS: ws, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: ws.root, ProgressEvery: time.Hour, Log: log.New(os.Stderr, "", 0)}
	r.Credentials = &Credentials{GW: gw, Log: r.Log}
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
	if got := strings.Join(ws.Events(), ","); got != "authorize ghs_item1,prepare,authorize ghs_item1,push" {
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
	ev := &Evaluator{Credentials: testCreds(r.GW), GW: r.GW, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, Log: r.Log}
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
			ev := &Evaluator{Credentials: testCreds(mg), GW: mg, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, Model: c.nodeEval, Log: r.Log}
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
			// the fixture goal has $9.00 left (budgetCents 1000, spentCents
			// 100): it binds whenever the model's own default is above it
			kindBudget, kindSource := kindModelBudget(nil, "build", c.wantBuild)
			amount, source := SessionBudget(kindBudget, kindSource, goalWith(900), 0)
			buildBudget := fmt.Sprintf("$%.2f (%s)", amount, source)
			for _, want := range []string{"item ship-it/1: build on " + build + ", budget " + buildBudget + "\n", "item ship-it/1: evaluate on " + eval + ": mesh/ship-it-1"} {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("node.log lacks %q:\n%s", want, logs.String())
				}
			}
			if len(mg.notes) == 0 || mg.notes[0] != "session started: mesh/ship-it-1 on "+build {
				t.Fatalf("first progress note: %q", mg.notes)
			}
			if got := row(item1)["evaluation"]; got != "Fine.\n\nevaluators: one — normal/low goal, no protected path\n(evaluated on "+eval+")" {
				t.Fatalf("evaluation = %q", got)
			}

			// a rebase works on the builder's branch, so on its model
			f.RebaseTokens = true
			f.WorkItems[item1]["status"], f.WorkItems[item1]["rebaseState"] = "review", "claimed"
			rb := &Rebaser{Credentials: testCreds(mg), GW: mg, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, Model: c.nodeBuild, ProgressEvery: time.Hour, Log: r.Log}
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

// validateYML declares an always step and two path-scoped ones: api's
// fails while api/broken exists.
const validateYML = `suites:
  - name: vet
    kind: lint
    command: sleep 0.02
    always: true
  - name: api
    kind: unit
    command: sleep 0.02 && test ! -e api/broken
    paths: ["api/**"]
  - name: web
    kind: unit
    command: sleep 0.02
    paths: ["web/**"]
`

// gitRun runs git in dir, failing the test on an error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFiles writes each file under dir; an empty body removes it.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if body == "" {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// originRepo is a bare origin whose main holds files, api/ and web/.
func originRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	gitRun(t, root, "init", "--quiet", "--bare", origin)
	seedDir := filepath.Join(root, "seed")
	gitRun(t, root, "clone", "--quiet", origin, seedDir)
	all := map[string]string{"api/api.go": "package api\n", "web/web.go": "package web\n"}
	for k, v := range files {
		all[k] = v
	}
	writeFiles(t, seedDir, all)
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "base")
	gitRun(t, seedDir, "push", "--quiet", "origin", "HEAD:main")
	return origin
}

// gitWS is fakeWS on a real checkout of origin: Prepare clones it onto
// the item's branch, ChangedPaths is Git's, and Push answers HEAD.
type gitWS struct {
	*fakeWS
	t      *testing.T
	origin string
	// pushedAt is when Push answered.
	pushedAt time.Time
}

func (w *gitWS) Prepare(ctx context.Context, spec WorkspaceSpec) (string, error) {
	dir, err := w.fakeWS.Prepare(ctx, spec)
	if err != nil {
		return "", err
	}
	gitRun(w.t, dir, "clone", "--quiet", w.origin, ".")
	gitRun(w.t, dir, "checkout", "--quiet", "-b", spec.Branch)
	return dir, nil
}

// AddRebaseWorktree clones origin at dir on the branch, from origin's
// when it has one, else from its default head: a real checkout to rebase
// in, as Prepare's.
func (w *gitWS) AddRebaseWorktree(ctx context.Context, repo, dir, branch string) error {
	if err := w.fakeWS.AddRebaseWorktree(ctx, repo, dir, branch); err != nil {
		return err
	}
	gitRun(w.t, dir, "clone", "--quiet", w.origin, ".")
	if gitRun(w.t, dir, "ls-remote", "--heads", "origin", branch) != "" {
		gitRun(w.t, dir, "checkout", "--quiet", "-b", branch, "origin/"+branch)
	} else {
		gitRun(w.t, dir, "checkout", "--quiet", "-b", branch)
	}
	return nil
}

func (w *gitWS) ChangedPaths(ctx context.Context, dir, base string) ([]string, error) {
	return (&Git{}).ChangedPaths(ctx, dir, base)
}

func (w *gitWS) Push(ctx context.Context, dir, branch string) (string, error) {
	if _, err := w.fakeWS.Push(ctx, dir, branch); err != nil {
		return "", err
	}
	sha := gitRun(w.t, dir, "rev-parse", "HEAD")
	w.mu.Lock()
	w.pushedAt = time.Now()
	w.mu.Unlock()
	return sha, nil
}

// commitHarness is a build session that commits turns[n] (files; "" a
// removal) on its n-th run, nothing past them, and succeeds.
type commitHarness struct {
	t     *testing.T
	mu    sync.Mutex
	jobs  []harness.Job
	turns []map[string]string
}

func (h *commitHarness) Name() string { return "claude-code" }

func (h *commitHarness) Run(_ context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	n := len(h.jobs)
	h.jobs = append(h.jobs, job)
	var files map[string]string
	if n < len(h.turns) {
		files = h.turns[n]
	}
	h.mu.Unlock()
	if files != nil {
		writeFiles(h.t, job.Workspace, files)
		gitRun(h.t, job.Workspace, "add", "-A")
		gitRun(h.t, job.Workspace, "commit", "--quiet", "-m", fmt.Sprintf("turn %d", n+1))
	}
	on(harness.Progress{SessionID: "s-1"})
	on(harness.Progress{Text: "committed", CostUSD: 0.5})
	return harness.Result{Success: true, Text: "Committed.", SessionID: "s-1", CostUSD: 0.5}, nil
}

func (h *commitHarness) Jobs() []harness.Job {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]harness.Job(nil), h.jobs...)
}

// validatingRunner is a runner on a real checkout of an origin whose
// main declares validateYML (files beside it), with h committing each
// turn. The fake takes the node's token alone, as the gateway does: an
// upload on the item's ghs_* token is refused.
func validatingRunner(t *testing.T, repoFiles map[string]string, turns ...map[string]string) (*Runner, *gitWS, *commitHarness, *gatewaytest.Fake, gateway.WorkItem, *syncBuf) {
	t.Helper()
	f := seed(t)
	h := &commitHarness{t: t, turns: turns}
	r, ws, item, _ := runnerOnFake(t, f, h, nil)
	gws := &gitWS{fakeWS: ws, t: t, origin: originRepo(t, repoFiles)}
	r.WS = gws
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	return r, gws, h, f, item, logs
}

func withValidation() map[string]string {
	return map[string]string{".runsheet/validate.yml": validateYML}
}

// attestationSteps are the suites of an uploaded attestation's commands.
func attestationSteps(t *testing.T, call gatewaytest.Call) []string {
	t.Helper()
	a, _ := call.Input["input"].(map[string]any)
	commands, _ := a["commands"].([]any)
	var names []string
	for _, c := range commands {
		names = append(names, c.(map[string]any)["suite"].(string))
	}
	return names
}

// pathsOnlyYML is a validate.yml every suite of which names paths: a
// change none of them match affects none.
const pathsOnlyYML = `suites:
  - name: api
    kind: unit
    command: sleep 0.02
    paths: ["api/**"]
  - name: web
    kind: unit
    command: sleep 0.02
    paths: ["web/**"]
  - name: e2e
    kind: integration
    command: sleep 0.02
    paths: ["e2e/**", "api/**"]
    jobs: [shard (e2e 1/3)]
`

// uploadedAttestation is the one recordValidationAttestation call's input.
func uploadedAttestation(t *testing.T, f *gatewaytest.Fake) map[string]any {
	t.Helper()
	atts := f.CallsNamed("recordValidationAttestation")
	if len(atts) != 1 {
		t.Fatalf("recordValidationAttestation calls = %d, want 1", len(atts))
	}
	a, _ := atts[0].Input["input"].(map[string]any)
	return a
}

// Rule (d): a diff no suite's paths match runs nothing — and still
// uploads one attestation, its commands empty and its suiteHash the
// file's, so the verify step answers on it and never "missing".
func TestAttestationUploadedWhenNothingIsAffected(t *testing.T) {
	r, _, _, f, item, _ := validatingRunner(t, map[string]string{".runsheet/validate.yml": pathsOnlyYML},
		map[string]string{"docs/notes.md": "a note\n"})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	a := uploadedAttestation(t, f)
	commands, ok := a["commands"].([]any)
	if !ok || len(commands) != 0 {
		t.Fatalf("commands = %#v, want an empty list", a["commands"])
	}
	want, err := validate.SuiteHash([]byte(pathsOnlyYML))
	if err != nil {
		t.Fatal(err)
	}
	if a["suiteHash"] != want {
		t.Fatalf("suiteHash = %v, want the file's %s", a["suiteHash"], want)
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 1 {
		t.Fatalf("finishWorkItem calls = %d", n)
	}
}

// A diff some suites' paths match runs those, and the attestation's
// commands name exactly them, in the file's order.
func TestAttestationNamesTheSubsetRun(t *testing.T) {
	r, _, _, f, item, _ := validatingRunner(t, map[string]string{".runsheet/validate.yml": pathsOnlyYML},
		map[string]string{"api/handler.go": "package api\n"})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	a := uploadedAttestation(t, f)
	var names []string
	for _, c := range a["commands"].([]any) {
		row := c.(map[string]any)
		names = append(names, row["suite"].(string))
		if row["exitCode"] != float64(0) {
			t.Fatalf("command = %v", row)
		}
	}
	if got := strings.Join(names, ","); got != "api,e2e" {
		t.Fatalf("commands name %s, want api,e2e", got)
	}
	want, _ := validate.SuiteHash([]byte(pathsOnlyYML))
	if a["suiteHash"] != want {
		t.Fatalf("suiteHash = %v, want %s", a["suiteHash"], want)
	}
}

// A build whose commit touches api/ runs the always step and api's —
// not web's — in the file's order, and finishes.
func TestBuildRunsOnlyTheAffectedStepsPlusAlways(t *testing.T) {
	r, _, _, f, item, _ := validatingRunner(t, withValidation(), map[string]string{"api/api.go": "package api\n\nconst X = 1\n"})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	atts := f.CallsNamed("recordValidationAttestation")
	if len(atts) != 1 {
		t.Fatalf("attestations = %d, want 1", len(atts))
	}
	if got := strings.Join(attestationSteps(t, atts[0]), ","); got != "vet,api" {
		t.Fatalf("steps run = %s, want vet,api", got)
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 1 {
		t.Fatalf("finishWorkItem calls = %d", n)
	}
}

// The attestation is uploaded after the push, for the pushed commit,
// with every field, on the node's own token — never the item's ghs_*
// token, which the gateway refuses.
func TestBuildUploadsTheAttestationOnTheNodesToken(t *testing.T) {
	r, ws, _, f, item, _ := validatingRunner(t, withValidation(), map[string]string{"web/web.go": "package web\n\nconst Y = 2\n"})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	atts := f.CallsNamed("recordValidationAttestation")
	if len(atts) != 1 {
		t.Fatalf("attestations = %d, want 1", len(atts))
	}
	call := atts[0]
	if call.Bearer != f.Token {
		t.Fatalf("bearer = %q, want the node's own token %q", call.Bearer, f.Token)
	}
	finish := f.CallsNamed("finishWorkItem")
	if len(finish) != 1 {
		t.Fatalf("finishWorkItem calls = %d", len(finish))
	}
	sha, _ := finish[0].Input["headSha"].(string)
	if len(sha) != 40 {
		t.Fatalf("headSha = %q", sha)
	}
	ws.mu.Lock()
	pushedAt := ws.pushedAt
	ws.mu.Unlock()
	if pushedAt.IsZero() || call.At.Before(pushedAt) {
		t.Fatalf("uploaded at %v, pushed at %v: the upload must follow the push", call.At, pushedAt)
	}
	a, _ := call.Input["input"].(map[string]any)
	if a["commit"] != sha {
		t.Fatalf("commit = %v, pushed %s", a["commit"], sha)
	}
	if call.Input["workItemId"] != item1 {
		t.Fatalf("workItemId = %v, want %s", call.Input["workItemId"], item1)
	}
	if tree, _ := a["treeHash"].(string); len(tree) != 40 || tree == sha {
		t.Fatalf("treeHash = %v", a["treeHash"])
	}
	host, _ := os.Hostname()
	if a["runnerImage"] != "host:"+host {
		t.Fatalf("runnerImage = %v", a["runnerImage"])
	}
	if hash, _ := a["suiteHash"].(string); a["sessionId"] != "s-1" || len(hash) != 64 {
		t.Fatalf("attestation = %v", a)
	}
	for _, gone := range []string{"nodeId", "itemId", "tree", "sandboxed", "steps"} {
		if _, ok := a[gone]; ok {
			t.Fatalf("attestation carries %q: %v", gone, a)
		}
	}
	if at, _ := a["at"].(string); at == "" {
		t.Fatalf("at = %v", a["at"])
	} else if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
		t.Fatalf("at = %v: %v", at, err)
	}
	commands, _ := a["commands"].([]any)
	if len(commands) != 2 {
		t.Fatalf("commands = %v", commands)
	}
	for _, c := range commands {
		row := c.(map[string]any)
		for _, k := range []string{"suite", "command", "exitCode", "durationMs"} {
			if _, ok := row[k]; !ok {
				t.Fatalf("command %v lacks %s", row, k)
			}
		}
		if row["exitCode"] != float64(0) || row["command"] == "" {
			t.Fatalf("command = %v", row)
		}
	}
}

// With no sandbox the attestation names the host and says unsandboxed.
func TestUnsandboxedRunSaysHostInRunnerImage(t *testing.T) {
	r, _, _, f, item, _ := validatingRunner(t, withValidation(), map[string]string{"api/api.go": "package api\n// x\n"})
	if r.Sandbox != nil {
		t.Fatal("want no sandbox")
	}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	atts := f.CallsNamed("recordValidationAttestation")
	if len(atts) != 1 {
		t.Fatalf("attestations = %d", len(atts))
	}
	a, _ := atts[0].Input["input"].(map[string]any)
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if a["runnerImage"] != "host:"+host {
		t.Fatalf("runnerImage = %v", a["runnerImage"])
	}
}

// A failing step resumes the session once with a prompt naming it; a
// retry that still fails fails the item naming the step, uploads that
// run's attestation, and finishes nothing.
func TestFailingStepGivesOneRetryTurnThenFailsNamingTheStep(t *testing.T) {
	r, ws, h, f, item, _ := validatingRunner(t, withValidation(),
		map[string]string{"api/broken": "x"},
		map[string]string{"api/other.go": "package api\n"})
	err := r.RunItem(context.Background(), item)
	if err == nil || !strings.HasPrefix(err.Error(), "validation failed: step api") {
		t.Fatalf("err = %v", err)
	}
	jobs := h.Jobs()
	if len(jobs) != 2 {
		t.Fatalf("sessions = %d, want the build and one retry", len(jobs))
	}
	if jobs[1].Resume != "s-1" {
		t.Fatalf("retry resume = %q, want s-1", jobs[1].Resume)
	}
	for _, want := range []string{"step api", "test ! -e api/broken", "exited 1", "commit what works", "Never push"} {
		if !strings.Contains(jobs[1].Prompt, want) {
			t.Fatalf("retry prompt lacks %q:\n%s", want, jobs[1].Prompt)
		}
	}
	if jobs[1].Outlive || jobs[1].HandBackAt != 0 {
		t.Fatalf("retry job = %+v", jobs[1])
	}
	fails := f.CallsNamed("failWorkItem")
	if len(fails) != 1 {
		t.Fatalf("failWorkItem calls = %d", len(fails))
	}
	if msg, _ := fails[0].Input["error"].(string); !strings.HasPrefix(msg, "validation failed: step api exited 1") {
		t.Fatalf("fail reason = %q", msg)
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 0 {
		t.Fatalf("finishWorkItem called %d times", n)
	}
	if len(ws.pushed) != 0 {
		t.Fatalf("pushed %v", ws.pushed)
	}
	atts := f.CallsNamed("recordValidationAttestation")
	if len(atts) != 1 {
		t.Fatalf("attestations = %d, want the failed run's", len(atts))
	}
	a, _ := atts[0].Input["input"].(map[string]any)
	commands, _ := a["commands"].([]any)
	if last := commands[len(commands)-1].(map[string]any); last["suite"] != "api" || last["exitCode"] != float64(1) {
		t.Fatalf("commands = %v", commands)
	}
	var noted bool
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		if n, _ := c.Input["note"].(string); strings.HasPrefix(n, "validation failed: step api") {
			noted = true
		}
	}
	if !noted {
		t.Fatal("the failure is not reported on the item")
	}
}

// On a platform holding changesets a failed build records no changeset,
// and the platform holds a changeset item's attestation to its recorded
// changeset's tree: the failed run's upload was refused every time
// (dl360, 2026-10-25: "GitHub has none", or wrong_tree against the
// previous attempt's), so it is not sent — the item still fails naming
// the step, and the log says why nothing was attested.
func TestFailedChangesetBuildSendsNoAttestation(t *testing.T) {
	r, _, _, f, item, logs := validatingRunner(t, withValidation(), map[string]string{"api/broken": "x"})
	f.Changesets = true
	err := r.RunItem(context.Background(), item)
	if err == nil || !strings.HasPrefix(err.Error(), "validation failed: step api") {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.CallsNamed("failWorkItem")); n != 1 {
		t.Fatalf("failWorkItem calls = %d", n)
	}
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 0 {
		t.Fatalf("attestations = %d, want none for a failed changeset build", n)
	}
	if !strings.Contains(logs.String(), "no changeset is recorded for it, so no attestation is sent") {
		t.Fatalf("log = %s", logs.String())
	}
}

// A retry turn that lands no new commit is the same failure.
func TestRetryWithoutACommitFailsNamingTheStep(t *testing.T) {
	r, _, h, f, item, _ := validatingRunner(t, withValidation(), map[string]string{"api/broken": "x"})
	if err := r.RunItem(context.Background(), item); err == nil {
		t.Fatal("want the failure")
	}
	if n := len(h.Jobs()); n != 2 {
		t.Fatalf("sessions = %d", n)
	}
	fails := f.CallsNamed("failWorkItem")
	if len(fails) != 1 {
		t.Fatalf("failWorkItem calls = %d", len(fails))
	}
	if msg, _ := fails[0].Input["error"].(string); !strings.HasPrefix(msg, "validation failed: step api") {
		t.Fatalf("fail reason = %q", msg)
	}
}

// A retry that fixes the step is validated again and finishes on its
// commit.
func TestFailingStepFixedOnTheRetryFinishes(t *testing.T) {
	r, _, h, f, item, _ := validatingRunner(t, withValidation(),
		map[string]string{"api/broken": "x"},
		map[string]string{"api/broken": ""})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if n := len(h.Jobs()); n != 2 {
		t.Fatalf("sessions = %d", n)
	}
	if n := len(f.CallsNamed("failWorkItem")); n != 0 {
		t.Fatalf("failWorkItem called %d times", n)
	}
	finish := f.CallsNamed("finishWorkItem")
	if len(finish) != 1 {
		t.Fatalf("finishWorkItem calls = %d", len(finish))
	}
	atts := f.CallsNamed("recordValidationAttestation")
	if len(atts) != 1 {
		t.Fatalf("attestations = %d", len(atts))
	}
	a, _ := atts[0].Input["input"].(map[string]any)
	if a["commit"] != finish[0].Input["headSha"] {
		t.Fatalf("attested %v, finished %v", a["commit"], finish[0].Input["headSha"])
	}
	for _, c := range a["commands"].([]any) {
		if c.(map[string]any)["exitCode"] != float64(0) {
			t.Fatalf("commands = %v", a["commands"])
		}
	}
}

// A repository with no validate.yml finishes as before: no attestation,
// one log line.
func TestNoValidationFileFinishesAsBefore(t *testing.T) {
	r, ws, h, f, item, logs := validatingRunner(t, nil, map[string]string{"api/api.go": "package api\n// y\n"})
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if n := len(h.Jobs()); n != 1 {
		t.Fatalf("sessions = %d", n)
	}
	if n := len(f.CallsNamed("recordValidationAttestation")); n != 0 {
		t.Fatalf("attestations = %d", n)
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 1 || len(ws.pushed) != 1 {
		t.Fatalf("finishes = %d, pushes = %v", n, ws.pushed)
	}
	if got := strings.Count(logs.String(), "no validation declared"); got != 1 {
		t.Fatalf("'no validation declared' logged %d times:\n%s", got, logs.String())
	}
}

// The finish's usage report carries one validation row per step run,
// its milliseconds and exit code.
func TestPerStepMinutesRideTheUsageRecord(t *testing.T) {
	r, _, _, f, item, _ := validatingRunner(t, withValidation(), map[string]string{"api/api.go": "package api\n// z\n"})
	r.Credentials = &Credentials{GW: r.GW.(*gateway.Client)}
	issueClaude(f, item.ID)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	finish := f.CallsNamed("finishWorkItem")
	if len(finish) != 1 {
		t.Fatalf("finishWorkItem calls = %d", len(finish))
	}
	usage, _ := finish[0].Input["usage"].(map[string]any)
	rows, _ := usage["validation"].([]any)
	if len(rows) != 2 {
		t.Fatalf("usage.validation = %v", usage["validation"])
	}
	for i, want := range []string{"vet", "api"} {
		row := rows[i].(map[string]any)
		if row["step"] != want {
			t.Fatalf("row %d = %v, want %s", i, row, want)
		}
		if ms, _ := row["ms"].(float64); ms <= 0 {
			t.Fatalf("row %d ms = %v", i, row["ms"])
		}
	}
}

// A platform whose usage report takes no validation still gets the
// finish, said once.
func TestNoValidationUsageFinishesSaidOnce(t *testing.T) {
	r, _, _, f, item, logs := validatingRunner(t, withValidation(), map[string]string{"api/api.go": "package api\n// w\n"})
	f.NoValidationUsage = true
	r.Credentials = &Credentials{GW: r.GW.(*gateway.Client)}
	issueClaude(f, item.ID)
	r.Notices = &ValidationNotices{}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 1 {
		t.Fatalf("finishWorkItem calls = %d", n)
	}
	if got := strings.Count(logs.String(), "takes no per-step validation minutes"); got != 1 {
		t.Fatalf("notice said %d times:\n%s", got, logs.String())
	}
}

// With a sandbox each step runs in the session's pod, with the
// workspace, and the attestation names the image's digest, sandboxed.
func TestSandboxedRunRunsEachStepInThePod(t *testing.T) {
	r, _, _, f, item, _ := validatingRunner(t, withValidation(), map[string]string{"api/api.go": "package api\n// s\n"})
	rt := &sandbox.Fake{}
	rt.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		cmd := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = spec.Workdir, spec.Stdout, spec.Stderr
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return sandbox.Exit{Code: ee.ExitCode()}, nil
		}
		return sandbox.Exit{}, err
	}
	r.Sandbox = &sandbox.Sandbox{Runtime: rt, Config: sandbox.Config{Image: "ghcr.io/runsheet/mesh-node:v1"}, TempDir: t.TempDir()}
	r.Inspect = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "podman" || strings.Join(args, " ") != "image inspect --format {{.Digest}} ghcr.io/runsheet/mesh-node:v1" {
			return nil, fmt.Errorf("unexpected %s %v", name, args)
		}
		return []byte("sha256:feedface\n"), nil
	}
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	var ran []string
	for _, a := range rt.Agents {
		if len(a.Command) == 3 && a.Command[0] == "sh" {
			ran = append(ran, a.Command[2])
		}
	}
	if len(ran) != 2 || ran[0] != "sleep 0.02" || !strings.Contains(ran[1], "api/broken") {
		t.Fatalf("steps in the pod = %v", ran)
	}
	atts := f.CallsNamed("recordValidationAttestation")
	if len(atts) != 1 {
		t.Fatalf("attestations = %d", len(atts))
	}
	a, _ := atts[0].Input["input"].(map[string]any)
	if a["runnerImage"] != "sha256:feedface" {
		t.Fatalf("runnerImage = %v", a["runnerImage"])
	}
}

// A build says what its context cost in one whole node.log line: the
// bytes on disk under .mesh/context (INDEX.md among them), the document
// files, and the indexed documents left to fetch (adr-016).
func TestBuildLogsItsContextWritten(t *testing.T) {
	h := &fakeHarness{}
	f := seed(t)
	// a skill is indexed, never on disk: one more document to fetch
	f.Documents["acme/mesh"] = append(f.Documents["acme/mesh"], map[string]any{"slug": "review", "title": "Review", "kind": "skill", "repo": "acme/widgets", "path": "skills/review.md"})
	r, ws, item, _ := runnerOnFake(t, f, h, nil)
	var logs strings.Builder
	r.Log = log.New(&logs, "", 0)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(ws.prepared) != 1 {
		t.Fatalf("prepared: %+v", ws.prepared)
	}
	cdir := filepath.Join(ws.prepared[0].Dir(), ContextDir)
	entries, err := os.ReadDir(cdir)
	if err != nil {
		t.Fatal(err)
	}
	bytes, files := 0, 0
	for _, e := range entries {
		st, err := os.Stat(filepath.Join(cdir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		bytes += int(st.Size())
		if e.Name() != IndexFile {
			files++
		}
	}
	fetch := len(f.Documents["acme/mesh"]) - files
	want := fmt.Sprintf("item ship-it/1: context %d bytes in %d files, %d documents to fetch\n", bytes, files, fetch)
	if bytes == 0 || files == 0 || fetch < 1 || strings.Count(logs.String(), ": context ") != 1 || !strings.Contains("\n"+logs.String(), "\n"+want) {
		t.Fatalf("node.log lacks the line %q once:\n%s", want, logs.String())
	}
}

// A build whose claim grants a pin — one exact, create-only ref — pushes
// its branch and then the pin, both at the head it validated, and
// finishes saying the pin ({pin: {ref}}); it records no changeset of its
// own, the product reading its result from the pin (runsheet ADR 076,
// option (c)). A claim granting none is finished as before.
func TestBuildPushesTheClaimsPinAndSaysSo(t *testing.T) {
	const pin = "refs/runsheet/changesets/0f0e0d0c-0b0a-4908-8706-050403020100"
	f := seed(t)
	f.KindGit = map[string]map[string]any{"runsheet/build": {"repo": "acme/widgets", "access": "write", "push": []any{
		map[string]any{"ref": "refs/heads/mesh/ship-it-1", "create_only": false},
		map[string]any{"ref": pin, "create_only": true},
	}}}
	h := &fakeHarness{text: "done"}
	r, ws, item, row := runnerOnFake(t, f, h, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ws.events, "push") || !slices.Contains(ws.events, "pin "+pin) || slices.Index(ws.events, "pin "+pin) < slices.Index(ws.events, "push") {
		t.Fatalf("events: %v, want the branch push then the pin", ws.events)
	}
	fin := f.CallsNamed("finishJob")
	if len(fin) != 1 {
		t.Fatalf("finishJob calls: %+v", fin)
	}
	finish, _ := fin[0].Input["finish"].(map[string]any)
	if p, _ := finish["pin"].(map[string]any); p["ref"] != pin {
		t.Fatalf("finish: %+v, want pin %s", finish, pin)
	}
	if n := len(f.CallsNamed("recordChangeset")); n != 0 {
		t.Fatalf("the node recorded a changeset of its own: %d", n)
	}
	if row(item1)["status"] != "evaluating" {
		t.Fatalf("item: %v", row(item1))
	}
}
