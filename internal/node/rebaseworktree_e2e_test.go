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
