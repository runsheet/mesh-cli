//go:build e2e

package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A rebase runs in a fresh checkout when the item's own is gone: against
// a local bare origin (lockTestOrigin), an item's worktree is prepared,
// built and pushed, then its directory deleted; AddRebaseWorktree makes a
// detached worktree of the shared clone at origin/<branch>, and a commit
// made there pushes to the branch on the origin.
func TestRebaseWorktreeDeletedCheckoutE2E(t *testing.T) {
	root, origin := lockTestOrigin(t)
	work, repos := filepath.Join(root, "work"), filepath.Join(root, "repos")
	g, cmds := recordingGit(repos)
	ctx := context.Background()

	spec := WorkspaceSpec{Root: work, GoalSlug: "g", Seq: 1, Repo: origin, Branch: BranchName("g", 1), BaseRef: "main"}
	dir, err := g.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "item"), []byte("built\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, dir, "add", "item")
	testGit(t, dir, "commit", "-q", "-m", "build")
	built, err := g.Push(ctx, dir, spec.Branch)
	if err != nil {
		t.Fatalf("push the build: %v", err)
	}

	// the item's checkout goes, its registration in the shared clone left
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	rdir := filepath.Join(work, "g", "1-rebase")
	if err := g.AddRebaseWorktree(ctx, origin, rdir, spec.Branch); err != nil {
		t.Fatalf("AddRebaseWorktree with the item's checkout deleted: %v\n%s", err, strings.Join(cmds(), "\n"))
	}
	if head := testGit(t, rdir, "rev-parse", "HEAD"); head != built {
		t.Fatalf("rebase worktree HEAD %s, want origin/%s %s", head, spec.Branch, built)
	}
	if want := testGit(t, rdir, "rev-parse", "refs/remotes/origin/"+spec.Branch); want != built {
		t.Fatalf("origin/%s is %s, want %s", spec.Branch, want, built)
	}

	if err := os.WriteFile(filepath.Join(rdir, "rebased"), []byte("rebased\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, rdir, "add", "rebased")
	testGit(t, rdir, "commit", "-q", "-m", "rebase")
	head := testGit(t, rdir, "rev-parse", "HEAD")
	testGit(t, rdir, "push", "-q", "origin", "HEAD:refs/heads/"+spec.Branch)

	out, err := exec.Command("git", "--git-dir", origin, "rev-parse", "refs/heads/"+spec.Branch).Output()
	if err != nil {
		t.Fatalf("read the origin's %s: %v", spec.Branch, err)
	}
	if got := strings.TrimSpace(string(out)); got != head {
		t.Fatalf("the origin's %s is %s, want the rebase commit %s", spec.Branch, got, head)
	}
	t.Logf("rebase worktree %s pushed %s to %s (was %s)", rdir, head, spec.Branch, built)
}

// A stale directory at the rebase path is replaced: a plain dir holding a
// junk file, and a leftover rebase worktree whose registration in the
// shared clone is gone, each give way to a working checkout at
// origin/<branch>, the junk gone.
func TestRebaseWorktreeReplacesStaleDirE2E(t *testing.T) {
	root, origin := lockTestOrigin(t)
	work, repos := filepath.Join(root, "work"), filepath.Join(root, "repos")
	g, cmds := recordingGit(repos)
	ctx := context.Background()

	spec := WorkspaceSpec{Root: work, GoalSlug: "g", Seq: 1, Repo: origin, Branch: BranchName("g", 1), BaseRef: "main"}
	dir, err := g.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "item"), []byte("built\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, dir, "add", "item")
	testGit(t, dir, "commit", "-q", "-m", "build")
	built, err := g.Push(ctx, dir, spec.Branch)
	if err != nil {
		t.Fatalf("push the build: %v", err)
	}
	rdir := filepath.Join(work, "g", "1-rebase")

	replaced := func(t *testing.T) {
		t.Helper()
		if err := g.AddRebaseWorktree(ctx, origin, rdir, spec.Branch); err != nil {
			t.Fatalf("AddRebaseWorktree over a stale dir: %v\n%s", err, strings.Join(cmds(), "\n"))
		}
		if _, err := os.Stat(filepath.Join(rdir, "junk")); !os.IsNotExist(err) {
			t.Fatalf("junk still at %s (stat: %v)", rdir, err)
		}
		if head := testGit(t, rdir, "rev-parse", "HEAD"); head != built {
			t.Fatalf("rebase worktree HEAD %s, want origin/%s %s", head, spec.Branch, built)
		}
		if st := testGit(t, rdir, "status", "--porcelain"); st != "" {
			t.Fatalf("rebase worktree not clean:\n%s", st)
		}
		if b, err := os.ReadFile(filepath.Join(rdir, "item")); err != nil || string(b) != "built\n" {
			t.Fatalf("item file %q, %v; want the build's", b, err)
		}
	}

	t.Run("plain dir", func(t *testing.T) {
		if err := os.MkdirAll(rdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rdir, "junk"), []byte("stale\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		replaced(t)
	})

	t.Run("worktree whose registration is gone", func(t *testing.T) {
		// rdir is a rebase worktree from the subtest above; its admin in
		// the shared clone goes, and junk is left in its files
		admin := worktreeAdmin(rdir)
		if admin == "" {
			t.Fatalf("%s names no worktree admin", rdir)
		}
		if err := os.RemoveAll(admin); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rdir, "junk"), []byte("stale\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		replaced(t)
	})

	// the item's own worktree was never touched: no blanket prune
	if head := testGit(t, dir, "rev-parse", "HEAD"); head != built {
		t.Fatalf("the item's worktree HEAD %s, want %s", head, built)
	}
}

// buildAndPush prepares the item's worktree, commits a file in it and
// pushes the branch, as a build does; it returns the checkout and the
// pushed head.
func buildAndPush(t *testing.T, g *Git, spec WorkspaceSpec) (string, string) {
	t.Helper()
	ctx := context.Background()
	dir, err := g.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "item"), []byte("built\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, dir, "add", "item")
	testGit(t, dir, "commit", "-q", "-m", "build")
	built, err := g.Push(ctx, dir, spec.Branch)
	if err != nil {
		t.Fatalf("push the build: %v", err)
	}
	return dir, built
}

// rebasePushes checks rdir is a clean checkout at built, commits in it
// and pushes HEAD to the branch, then reads the origin's branch back as
// that commit.
func rebasePushes(t *testing.T, origin, rdir, branch, built string) string {
	t.Helper()
	if head := testGit(t, rdir, "rev-parse", "HEAD"); head != built {
		t.Fatalf("rebase worktree HEAD %s, want origin/%s %s", head, branch, built)
	}
	if st := testGit(t, rdir, "status", "--porcelain"); st != "" {
		t.Fatalf("rebase worktree not clean:\n%s", st)
	}
	if err := os.WriteFile(filepath.Join(rdir, "rebased"), []byte("rebased\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, rdir, "add", "rebased")
	testGit(t, rdir, "commit", "-q", "-m", "rebase")
	head := testGit(t, rdir, "rev-parse", "HEAD")
	testGit(t, rdir, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
	out, err := exec.Command("git", "--git-dir", origin, "rev-parse", "refs/heads/"+branch).Output()
	if err != nil {
		t.Fatalf("read the origin's %s: %v", branch, err)
	}
	if got := strings.TrimSpace(string(out)); got != head {
		t.Fatalf("the origin's %s is %s, want the rebase commit %s", branch, got, head)
	}
	return head
}

// The 2026-10-17 06:32Z failure on PR #4236: the item's checkout is
// there, but its `.git` file names a `.git/worktrees/<n>` that is gone,
// so any git run inside it is `fatal: not a git repository`.
// AddRebaseWorktree runs nothing there and still makes a working
// checkout at origin/<branch>, whose commit pushes to the branch.
func TestRebaseWorktreeDanglingGitFileE2E(t *testing.T) {
	root, origin := lockTestOrigin(t)
	work, repos := filepath.Join(root, "work"), filepath.Join(root, "repos")
	g, cmds := recordingGit(repos)
	ctx := context.Background()

	spec := WorkspaceSpec{Root: work, GoalSlug: "g", Seq: 1, Repo: origin, Branch: BranchName("g", 1), BaseRef: "main"}
	// a worktree made before checkouts were clones of their own
	if _, err := prepareWorktree(ctx, g, spec); err != nil {
		t.Fatal(err)
	}
	dir, built := buildAndPush(t, g, spec)

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
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").CombinedOutput(); err == nil {
		t.Fatalf("git still works in the dead checkout: %s", out)
	}

	rdir := filepath.Join(work, "g", "1-rebase")
	before := len(cmds())
	if err := g.AddRebaseWorktree(ctx, origin, rdir, spec.Branch); err != nil {
		t.Fatalf("AddRebaseWorktree with a dangling .git file at the item's checkout: %v\n%s", err, strings.Join(cmds(), "\n"))
	}
	for _, c := range cmds()[before:] {
		if strings.Contains(c+" ", " "+dir+" ") || strings.Contains(c, dir+string(filepath.Separator)) {
			t.Fatalf("AddRebaseWorktree ran in the dead checkout: %s", c)
		}
	}
	head := rebasePushes(t, origin, rdir, spec.Branch, built)

	// the dead checkout is left as it was: its .git file still there
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("the item's .git file: %v", err)
	}
	t.Logf("rebase worktree %s pushed %s to %s past a dangling %s", rdir, head, spec.Branch, admin)
}

// The item's live worktree still has the branch checked out: git refuses
// a second checkout of it, so the rebase worktree is detached at
// origin/<branch>, its commit pushes to the branch, and the live
// worktree keeps its branch, head and files.
func TestRebaseWorktreeBranchHeldE2E(t *testing.T) {
	root, origin := lockTestOrigin(t)
	work, repos := filepath.Join(root, "work"), filepath.Join(root, "repos")
	g, cmds := recordingGit(repos)
	ctx := context.Background()

	spec := WorkspaceSpec{Root: work, GoalSlug: "g", Seq: 1, Repo: origin, Branch: BranchName("g", 1), BaseRef: "main"}
	dir, built := buildAndPush(t, g, spec)
	if b := testGit(t, dir, "symbolic-ref", "--short", "HEAD"); b != spec.Branch {
		t.Fatalf("the item's worktree is on %q, want %s", b, spec.Branch)
	}
	// an uncommitted edit in the live worktree must survive
	if err := os.WriteFile(filepath.Join(dir, "wip"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rdir := filepath.Join(work, "g", "1-rebase")
	if err := g.AddRebaseWorktree(ctx, origin, rdir, spec.Branch); err != nil {
		t.Fatalf("AddRebaseWorktree with the branch held by the live worktree: %v\n%s", err, strings.Join(cmds(), "\n"))
	}
	if out, err := exec.Command("git", "-C", rdir, "symbolic-ref", "-q", "HEAD").CombinedOutput(); err == nil {
		t.Fatalf("rebase worktree is on a branch, want detached: %s", out)
	}
	head := rebasePushes(t, origin, rdir, spec.Branch, built)

	// the live worktree is undisturbed
	if b := testGit(t, dir, "symbolic-ref", "--short", "HEAD"); b != spec.Branch {
		t.Fatalf("the live worktree moved to %q, want %s", b, spec.Branch)
	}
	if h := testGit(t, dir, "rev-parse", "HEAD"); h != built {
		t.Fatalf("the live worktree's HEAD %s, want %s", h, built)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "wip")); err != nil || string(b) != "wip\n" {
		t.Fatalf("the live worktree's wip %q, %v", b, err)
	}
	if _, _, gone := worktreeAdminGone(dir); gone {
		t.Fatalf("the live worktree's admin went")
	}
	t.Logf("rebase worktree %s pushed %s to %s while %s held it", rdir, head, spec.Branch, dir)
}
