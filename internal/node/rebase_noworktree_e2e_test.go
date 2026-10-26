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

// A rebase's checkout is a clone of its own (ADR-039): against a local
// bare origin, with gh's clone substituted by git's (lockTestOrigin), two
// items of one repo are built and pushed, their checkouts left live, and
// each is rebased: AddRebaseWorktree makes <work>/g/<seq>-rebase a clone
// with a `.git` directory of its own and no alternates, detached at
// origin/<branch>, whose commit pushes to the branch, and `git worktree
// list` on the shared clone names only the clone — before the rebases,
// between them and after; no `git worktree` command runs, and removing
// the rebase checkout leaves the shared clone as it was.
func TestRebaseRegistersNoWorktree(t *testing.T) {
	root, origin := lockTestOrigin(t)
	work, repos := filepath.Join(root, "work"), filepath.Join(root, "repos")
	g, cmds := recordingGit(repos)
	ctx := context.Background()
	clone := SharedClonePath(repos, origin)

	onlyTheClone := func(when string) {
		t.Helper()
		list := testGit(t, clone, "worktree", "list", "--porcelain")
		var trees []string
		for _, line := range strings.Split(list, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				trees = append(trees, p)
			}
		}
		if len(trees) != 1 || filepath.Clean(trees[0]) != filepath.Clean(clone) {
			t.Fatalf("%s: git worktree list on the shared clone names %q, want only %s", when, trees, clone)
		}
		if regs, err := os.ReadDir(filepath.Join(clone, ".git", "worktrees")); err == nil && len(regs) > 0 {
			t.Fatalf("%s: the shared clone holds %d worktree registration(s)", when, len(regs))
		}
	}

	type built struct {
		spec WorkspaceSpec
		dir  string
		head string
	}
	var items []built
	for seq := 1; seq <= 2; seq++ {
		spec := WorkspaceSpec{Root: work, GoalSlug: "g", Seq: seq, Repo: origin, Branch: BranchName("g", seq), BaseRef: "main"}
		dir, head := buildAndPush(t, g, spec)
		items = append(items, built{spec, dir, head})
	}
	onlyTheClone("before the rebases")

	before := len(cmds())
	for _, it := range items {
		rspec := RebaseWorkspaceSpec(it.spec)
		rdir := rspec.Dir()
		if err := g.AddRebaseWorktree(ctx, origin, rdir, it.spec.Branch); err != nil {
			t.Fatalf("AddRebaseWorktree %s: %v\n%s", it.spec.Branch, err, strings.Join(cmds(), "\n"))
		}
		onlyTheClone("after the rebase checkout of " + it.spec.Branch)
		if fi, err := os.Stat(filepath.Join(rdir, ".git")); err != nil || !fi.IsDir() || isWorktree(rdir) {
			t.Fatalf("%s is not a clone of its own: %v", rdir, err)
		}
		if _, err := os.Stat(filepath.Join(rdir, ".git", "objects", "info", "alternates")); err == nil {
			t.Fatalf("%s borrows another clone's objects", rdir)
		}
		if out, err := exec.Command("git", "-C", rdir, "symbolic-ref", "-q", "HEAD").CombinedOutput(); err == nil {
			t.Fatalf("%s is on a branch, want detached: %s", rdir, out)
		}
		rebasePushes(t, origin, rdir, it.spec.Branch, it.head)
		onlyTheClone("after the rebase of " + it.spec.Branch)

		// the item's live checkout keeps its branch and head
		if b := testGit(t, it.dir, "symbolic-ref", "--short", "HEAD"); b != it.spec.Branch {
			t.Fatalf("the item's checkout moved to %q, want %s", b, it.spec.Branch)
		}
		if h := testGit(t, it.dir, "rev-parse", "HEAD"); h != it.head {
			t.Fatalf("the item's checkout HEAD %s, want %s", h, it.head)
		}

		if err := g.RemoveWorkspace(ctx, rdir); err != nil {
			t.Fatalf("remove %s: %v", rdir, err)
		}
		if _, err := os.Stat(rdir); !os.IsNotExist(err) {
			t.Fatalf("%s still there after RemoveWorkspace (stat: %v)", rdir, err)
		}
		onlyTheClone("after removing " + rdir)
	}
	for _, c := range cmds()[before:] {
		if strings.HasPrefix(c, "git worktree ") {
			t.Fatalf("the rebases ran %q", c)
		}
		if strings.Contains(c, clone) {
			t.Fatalf("the rebases ran %q on the shared clone", c)
		}
	}
}
