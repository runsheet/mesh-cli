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

	"projectmesh.io/mesh-cli/internal/gateway"
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
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: 1}
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
	if len(h.jobs) != 1 || h.jobs[0].Env != nil {
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
