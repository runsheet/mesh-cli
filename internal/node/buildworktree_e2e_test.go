//go:build e2e

package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A build's checkout is a clone of its own (ADR-039): against a local
// bare origin, with gh's clone substituted by git's (lockTestOrigin), the
// node's Git prepares two items of one repo, each commits and pushes on
// its own branch, and `git worktree list` on the shared clone names only
// the clone — before the builds, between them and after — while each
// item's `.git` is a directory of its own with no alternates.
func TestBuildRegistersNoWorktree(t *testing.T) {
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

	for seq := 1; seq <= 2; seq++ {
		spec := WorkspaceSpec{Root: work, GoalSlug: "g", Seq: seq, Repo: origin, Branch: BranchName("g", seq), BaseRef: "main"}
		dir, err := g.Prepare(ctx, spec)
		if err != nil {
			t.Fatalf("prepare seq %d: %v", seq, err)
		}
		onlyTheClone("after prepare of seq " + spec.Branch)
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() || isWorktree(dir) {
			t.Fatalf("seq %d: %s is not a clone of its own: %v", seq, dir, err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".git", "objects", "info", "alternates")); err == nil {
			t.Fatalf("seq %d: the checkout borrows the shared clone's objects", seq)
		}
		if b := testGit(t, dir, "branch", "--show-current"); b != spec.Branch {
			t.Fatalf("seq %d: on %s, want %s", seq, b, spec.Branch)
		}
		if err := os.WriteFile(filepath.Join(dir, "item"), []byte(spec.Branch+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		testGit(t, dir, "add", "item")
		testGit(t, dir, "commit", "-q", "-m", "build "+spec.Branch)
		head := testGit(t, dir, "rev-parse", "HEAD")
		pushed, err := g.Push(ctx, dir, spec.Branch)
		if err != nil || pushed != head {
			t.Fatalf("push seq %d: %s, %v; committed %s", seq, pushed, err, head)
		}
		if got := testGit(t, origin, "rev-parse", "refs/heads/"+spec.Branch); got != head {
			t.Fatalf("origin's %s is %s, want %s", spec.Branch, got, head)
		}
		onlyTheClone("after the build of " + spec.Branch)
	}
	for _, c := range cmds() {
		if strings.HasPrefix(c, "git worktree ") {
			t.Fatalf("the builds ran %q", c)
		}
	}
}
