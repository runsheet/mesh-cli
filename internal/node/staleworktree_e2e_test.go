//go:build e2e

package node

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// staleWorktrees runs scripts/stale-worktrees.sh over root: one line per
// checkout whose .git names a gitdir that is gone.
func staleWorktrees(t *testing.T, root string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(file), "..", "..", "scripts", "stale-worktrees.sh")
	out, err := exec.Command("bash", script, root).CombinedOutput()
	if err != nil {
		t.Fatalf("stale-worktrees.sh %s: %v\n%s", root, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A checkout whose worktree is gone is made again and its session runs
// (ADR-028, One worktree per item; goal
// a-checkout-whose-worktree-is-gone-is-made-again): against a local bare
// origin served as GitHub, the node's own Git builds item 1 of a goal as a
// worktree of the repo's shared clone, pushes and finishes it. Between
// attempts the item's registration is removed from the clone
// (`rm -rf <clone>/.git/worktrees/<name>`), as a blanket prune did on
// dl360, and scripts/stale-worktrees.sh lists the checkout. Separately a
// second item's worktree, locked while its item is live, has its
// directory hidden while `git worktree prune` runs in the clone: its
// registration survives. The rework is claimed and run: its checkout is
// made again — the `made again` line — its session commits on top of the
// pushed head, the node pushes and finishes it, the evaluator passes it,
// node.log carries no `not a git repository`, and the script lists
// nothing.
func TestStaleWorktreeIsMadeAgainE2E(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, kv := range []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_TERMINAL_PROMPT=0"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}

	// GitHub: acme/widgets with main
	base := filepath.Join(root, "gh")
	origin := filepath.Join(base, "acme", "widgets.git")
	seedDir := filepath.Join(root, "seed")
	gitRun(t, root, "init", "--quiet", "--bare", "-b", "main", origin)
	gitRun(t, root, "init", "--quiet", "-b", "main", seedDir)
	writeFiles(t, seedDir, map[string]string{"api/api.go": "package api\n"})
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "main")
	gitRun(t, seedDir, "push", "--quiet", origin, "HEAD:main")

	f := seed(t)
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	issueClaude(f, item1)
	gw := gateway.New(f.URL(), "rs_test")
	if err := gw.ClaimWorkItem(context.Background(), item1, cfg.NodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuf{}
	lg := log.New(logs, "", 0)
	repos := filepath.Join(root, "repos")
	g := &Git{GitHubURL: "file://" + base, ReposRoot: repos, Logf: lg.Printf, Log: io.Discard}
	work := filepath.Join(root, "work")
	clone := SharedClonePath(repos, "acme/widgets")
	h := &commitHarness{t: t, turns: []map[string]string{
		{"api/one.go": "package api\n\nconst One = 1\n"},
		{"api/two.go": "package api\n\nconst Two = 2\n"},
	}}
	r := &Runner{GW: gw, Harness: h, WS: g, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: work, ProgressEvery: time.Hour, Log: lg,
		Credentials: &Credentials{GW: gw, Log: lg}}

	// attempt 1: built as a worktree of the shared clone, pushed, finished
	item, err := gw.WorkItem(context.Background(), item1)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RunItem(context.Background(), *item); err != nil {
		t.Fatalf("RunItem: %v\n%s", err, logs.String())
	}
	branch := BranchName("ship-it", 1)
	head1 := strings.Fields(gitRun(t, root, "ls-remote", "--heads", origin, branch) + " ")[0]
	if head1 == "" {
		t.Fatalf("origin has no %s after attempt 1\n%s", branch, logs.String())
	}
	dir := filepath.Join(work, "ship-it", "1")
	admin := worktreeAdmin(dir)
	if admin == "" || !strings.HasPrefix(admin, filepath.Join(clone, ".git", "worktrees")) {
		t.Fatalf("%s is not a worktree of %s (admin %q)", dir, clone, admin)
	}

	// a second item's worktree, locked while its item is live, survives
	// a prune run while its directory is out of view
	spec2 := WorkspaceSpec{Root: work, GoalSlug: "ship-it", Seq: 2, Repo: "acme/widgets", Branch: BranchName("ship-it", 2), BaseRef: "main"}
	dir2, err := g.Prepare(context.Background(), spec2)
	if err != nil {
		t.Fatalf("Prepare item 2: %v\n%s", err, logs.String())
	}
	hidden := filepath.Join(root, "hidden-2")
	if err := os.Rename(dir2, hidden); err != nil {
		t.Fatal(err)
	}
	gitRun(t, clone, "worktree", "prune")
	if err := os.Rename(hidden, dir2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worktreeAdmin(dir2)); err != nil {
		t.Fatalf("item 2's registration did not survive the prune: %v\n%s", err, gitRun(t, clone, "worktree", "list", "--porcelain"))
	}
	gitRun(t, dir2, "status", "--short")

	// between attempts item 1's registration goes, as a blanket prune
	// took it on dl360: the reading lists its checkout
	if err := os.RemoveAll(admin); err != nil {
		t.Fatal(err)
	}
	if got, want := staleWorktrees(t, work), dir+" -> "+admin; got != want {
		t.Fatalf("stale-worktrees.sh before the rework = %q, want %q", got, want)
	}

	// attempt 2: the evaluator failed it and the platform handed it back
	f.SetItem(item1, map[string]any{"status": "ready", "attempts": 1, "branch": branch})
	if err := gw.ClaimWorkItem(context.Background(), item1, cfg.NodeID, "claude-code"); err != nil {
		t.Fatal(err)
	}
	if item, err = gw.WorkItem(context.Background(), item1); err != nil {
		t.Fatal(err)
	}
	if err := r.RunItem(context.Background(), *item); err != nil {
		t.Fatalf("RunItem (rework): %v\n%s", err, logs.String())
	}
	if n := len(h.Jobs()); n != 2 {
		t.Fatalf("build sessions = %d, want 2\n%s", n, logs.String())
	}
	if got := staleWorktrees(t, work); got != "" {
		t.Fatalf("stale-worktrees.sh after the rework's Prepare = %q, want nothing", got)
	}

	// the rework pushed a commit on top of attempt 1's
	head2 := strings.Fields(gitRun(t, root, "ls-remote", "--heads", origin, branch) + " ")[0]
	if head2 == head1 {
		t.Fatalf("the rework pushed nothing: %s is still %s\n%s", branch, head1, logs.String())
	}
	if parent := gitRun(t, origin, "rev-parse", head2+"^"); parent != head1 {
		t.Fatalf("the rework's head %s has parent %s, want attempt 1's %s", head2, parent, head1)
	}
	finish := f.CallsNamed("finishWorkItem")
	if len(finish) != 2 || finish[1].Input["headSha"] != head2 {
		t.Fatalf("finishes = %v, want the rework's on %s\n%s", finish, head2, logs.String())
	}
	if len(f.CallsNamed("failWorkItem")) != 0 || len(f.CallsNamed("releaseWorkItem")) != 0 {
		t.Fatalf("item 1 was failed or released:\n%s", logs.String())
	}

	// and it is evaluated, on its own checkout of the branch
	ev := &Evaluator{GW: gw, Harness: &fakeHarness{text: "The rework holds.\nVERDICT: PASS"}, WS: g, Scopes: scopesOf(cfg), WorkRoot: work, Model: "sonnet",
		Thresholds: func() LimitsConfig { return LimitsConfig{} }, Credentials: &Credentials{GW: gw, Log: lg}, Log: lg}
	if item, err = gw.WorkItem(context.Background(), item1); err != nil {
		t.Fatal(err)
	}
	if item.Status != "evaluating" {
		t.Fatalf("item 1 is %s after the rework's finish, want evaluating", item.Status)
	}
	if err := ev.Evaluate(context.Background(), *item); err != nil {
		t.Fatalf("Evaluate: %v\n%s", err, logs.String())
	}
	if evals := f.CallsNamed("evaluateWorkItem"); len(evals) != 1 || evals[0].Input["passed"] != true {
		t.Fatalf("evaluations = %+v, want one passing\n%s", evals, logs.String())
	}

	out := logs.String()
	if line := "workspace " + dir + ": worktree " + admin + " is gone; made again for " + branch + " at " + head1; !strings.Contains(out, line) {
		t.Fatalf("node.log lacks %q:\n%s", line, out)
	}
	if strings.Contains(out, "not a git repository") {
		t.Fatalf("node.log carries `not a git repository`:\n%s", out)
	}
	t.Logf("node.log:\n%s", out)
}
