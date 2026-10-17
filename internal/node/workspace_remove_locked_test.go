package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// porcelainNames reports whether `git worktree list --porcelain` in clone
// has a worktree line for dir, by its path or its symlink-resolved one.
func porcelainNames(t *testing.T, clone, dir string) (bool, string) {
	t.Helper()
	out, err := exec.Command("git", "-C", clone, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("worktree list --porcelain: %v", err)
	}
	names := map[string]bool{dir: true}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(dir)); err == nil {
		names[filepath.Join(parent, filepath.Base(dir))] = true
	}
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok && names[p] {
			return true, string(out)
		}
	}
	return false, string(out)
}

// preparedLocked prepares item g/1 as a worktree of the shared clone and
// checks it is locked, returning the spec, its dir and the clone.
func preparedLocked(t *testing.T) (*Git, WorkspaceSpec, string, string) {
	t.Helper()
	root, origin := lockTestOrigin(t)
	work, repos := filepath.Join(root, "work"), filepath.Join(root, "repos")
	g, _ := recordingGit(repos)
	spec := WorkspaceSpec{Root: work, GoalSlug: "g", Seq: 1, Repo: origin, Branch: BranchName("g", 1), BaseRef: "main"}
	dir, err := g.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	admin := worktreeAdmin(dir)
	if admin == "" {
		t.Fatalf("%s is not a worktree", dir)
	}
	if _, err := os.Stat(filepath.Join(admin, "locked")); err != nil {
		t.Fatalf("the live item's worktree is not locked: %v", err)
	}
	clone := SharedClonePath(repos, origin)
	if named, list := porcelainNames(t, clone, dir); !named {
		t.Fatalf("the prepared worktree is not registered:\n%s", list)
	}
	return g, spec, dir, clone
}

// A locked worktree is unlocked and removed: no registration names its
// dir and the same branch prepares again.
func TestRemoveWorkspaceClearsALockedWorktree(t *testing.T) {
	g, spec, dir, clone := preparedLocked(t)
	if err := g.RemoveWorkspace(context.Background(), dir); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the removed worktree %s is still there: %v", dir, err)
	}
	if named, list := porcelainNames(t, clone, dir); named {
		t.Fatalf("a registration still names the removed worktree:\n%s", list)
	}
	if _, err := g.Prepare(context.Background(), spec); err != nil {
		t.Fatalf("prepare the same branch again: %v", err)
	}
}

// A locked registration whose dir is gone — git would neither prune nor
// remove it — is forgotten by RemoveWorkspace, and the branch prepares
// again.
func TestRemoveWorkspaceClearsALockedRegistrationWhoseDirIsGone(t *testing.T) {
	g, spec, dir, clone := preparedLocked(t)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if named, list := porcelainNames(t, clone, dir); !named {
		t.Fatalf("the registration went with the dir; nothing left to test:\n%s", list)
	}
	if err := g.RemoveWorkspace(context.Background(), dir); err != nil {
		t.Fatalf("remove after the rm: %v", err)
	}
	if named, list := porcelainNames(t, clone, dir); named {
		t.Fatalf("the locked registration of the gone dir survived:\n%s", list)
	}
	if _, err := g.Prepare(context.Background(), spec); err != nil {
		t.Fatalf("prepare the same branch again: %v", err)
	}
}
