//go:build e2e

package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// freshRebaseHarness is a rebase session that does what RebasePrompt
// asks, in job.Workspace: fetch, rebase the detached HEAD onto
// origin/<base>, push it to the branch with a lease, and end REBASED:.
type freshRebaseHarness struct {
	t            *testing.T
	branch, base string
	mu           sync.Mutex
	workspaces   []string
	head         string
}

func (h *freshRebaseHarness) Name() string { return "claude-code" }

func (h *freshRebaseHarness) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	h.mu.Lock()
	h.workspaces = append(h.workspaces, job.Workspace)
	h.mu.Unlock()
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = job.Workspace
		cmd.Env = append(os.Environ(), job.Env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	for _, args := range [][]string{
		{"fetch", "--quiet", "origin"},
		{"checkout", "--quiet", "--detach", "origin/" + h.branch},
		{"rebase", "--quiet", "origin/" + h.base},
		{"push", "--quiet", "--force-with-lease=" + h.branch + ":origin/" + h.branch, "origin", "HEAD:refs/heads/" + h.branch},
	} {
		if _, err := git(args...); err != nil {
			return harness.Result{Success: true, SessionID: "rb-1", Text: "REBASE FAILED: " + err.Error()}, nil
		}
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return harness.Result{}, err
	}
	h.mu.Lock()
	h.head = head
	h.mu.Unlock()
	on(harness.Progress{SessionID: "rb-1", CostUSD: 0.1})
	return harness.Result{Success: true, SessionID: "rb-1", CostUSD: 0.1,
		Text: fmt.Sprintf("No conflicts left.\n\nREBASED: %s\nRESOLVED: (none)", head[:12])}, nil
}

// A rebase runs in a fresh checkout (goal
// a-rebase-session-runs-in-a-fresh-checkout; ADR-028, A rebase gets its
// own worktree): against a local bare origin served as GitHub, the
// node's own Git builds item 1 in its worktree <work>/ship-it/1 and
// pushes it; main moves on; then the item's checkout is broken — deleted
// outright, or left with a `.git` naming a pruned worktree registration
// (the 2026-10-17 06:32Z failure on PR #4236) — and the rebase is
// claimed. The session runs in <work>/ship-it/1-rebase, not the item's
// checkout, rebases the branch onto the moved main and pushes it;
// FinishWorkItemRebase is recorded with origin's new head, and the
// rebase checkout is gone after.
func TestRebaseRunsInAFreshCheckout(t *testing.T) {
	for _, c := range []struct {
		name    string
		breakIt func(t *testing.T, dir string)
		left    bool // the item's dir is still there after
	}{
		{"deleted", func(t *testing.T, dir string) {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"dangling .git", func(t *testing.T, dir string) {
			admin := worktreeAdmin(dir)
			if admin == "" {
				t.Fatalf("%s names no worktree admin", dir)
			}
			if err := os.RemoveAll(admin); err != nil {
				t.Fatal(err)
			}
			if _, _, gone := worktreeAdminGone(dir); !gone {
				t.Fatalf("%s: admin %s removed but not read as gone", dir, admin)
			}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			rebaseInAFreshCheckout(t, c.breakIt, c.left)
		})
	}
}

func rebaseInAFreshCheckout(t *testing.T, breakCheckout func(*testing.T, string), left bool) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

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

	logs := &syncBuf{}
	g := &Git{GitHubURL: "file://" + base, ReposRoot: filepath.Join(root, "repos"), Log: logs}
	work := filepath.Join(root, "work")

	// item 1 built in its own worktree and pushed
	branch := BranchName("ship-it", 1)
	spec := WorkspaceSpec{Root: work, GoalSlug: "ship-it", Seq: 1, Repo: "acme/widgets", Branch: branch, BaseRef: "main"}
	if err := g.Authorize(ctx, spec, &gateway.GitHubToken{Token: "ghs_build"}); err != nil {
		t.Fatalf("authorize the build: %v", err)
	}
	dir, err := g.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("prepare the build: %v", err)
	}
	if dir != spec.Dir() || !isWorktree(dir) {
		t.Fatalf("the build's checkout %s is not a worktree at %s", dir, spec.Dir())
	}
	writeFiles(t, dir, map[string]string{"api/item.go": "package api\n\nconst Item = 1\n"})
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "--quiet", "-m", "the item")
	built, err := g.Push(ctx, dir, branch)
	if err != nil {
		t.Fatalf("push the build: %v", err)
	}

	// main moves on, so the branch needs a rebase
	writeFiles(t, seedDir, map[string]string{"web/web.go": "package web\n"})
	gitRun(t, seedDir, "add", "-A")
	gitRun(t, seedDir, "commit", "--quiet", "-m", "main moves on")
	gitRun(t, seedDir, "push", "--quiet", origin, "HEAD:main")
	mainHead := gitRun(t, seedDir, "rev-parse", "HEAD")

	// the item's checkout breaks, and the platform asks for a rebase
	breakCheckout(t, dir)
	f := seed(t)
	requestRebase(f, item1, branch)
	h := &freshRebaseHarness{t: t, branch: branch, base: "main"}
	loop, _ := rebaseNodeOn(t, f, "test-node", h, g, work, logs)
	loop.Tick(ctx)
	loop.wg.Wait()

	if n := len(f.CallsNamed("claimWorkItemRebase")); n != 1 {
		t.Fatalf("rebase claims = %d, want 1\n%s", n, logs.String())
	}
	rdir := RebaseWorkspaceSpec(spec).Dir()
	if len(h.workspaces) != 1 {
		t.Fatalf("sessions = %d, want 1\n%s", len(h.workspaces), logs.String())
	}
	if ws := h.workspaces[0]; ws == dir || ws != rdir {
		t.Fatalf("the session ran in %s; want %s, never the item's checkout %s", ws, rdir, dir)
	}

	// what origin has is the rebased head: past main's, not the build's
	head := strings.Fields(gitRun(t, root, "ls-remote", "--heads", origin, branch) + " ")[0]
	if head == "" || head == built || head != h.head {
		t.Fatalf("origin's %s is %q (built %s, the session pushed %s)\n%s", branch, head, built, h.head, logs.String())
	}
	if _, err := exec.Command("git", "--git-dir", origin, "merge-base", "--is-ancestor", mainHead, head).CombinedOutput(); err != nil {
		t.Fatalf("origin's %s (%s) is not on main %s", branch, short(head), short(mainHead))
	}
	finishes := f.CallsNamed("finishWorkItemRebase")
	if len(finishes) != 1 || finishes[0].Input["headSha"] != head {
		t.Fatalf("finishes: %+v\n%s", finishes, logs.String())
	}
	if n := len(f.CallsNamed("failWorkItemRebase")); n != 0 {
		t.Fatalf("failWorkItemRebase called %d time(s)\n%s", n, logs.String())
	}
	if it := f.Item(item1); it["rebaseState"] != "rebased" {
		t.Fatalf("item: %v", it)
	}

	// the rebase's checkout goes once the rebase is over; the item's is
	// left as the rebase found it
	if _, err := os.Stat(rdir); !os.IsNotExist(err) {
		t.Fatalf("the rebase checkout %s is still there: %v", rdir, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); left != (err == nil) {
		t.Fatalf("the item's checkout %s: .git %v, want left=%v", dir, err, left)
	}
	t.Logf("rebased %s from %s to %s in %s", branch, short(built), short(head), rdir)
}
