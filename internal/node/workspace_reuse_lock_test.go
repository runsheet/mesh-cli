package node

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// worktreeLock reads clone's `git worktree list --porcelain` for dir:
// whether it is listed, whether it is locked, and the lock's reason.
func worktreeLock(t *testing.T, clone, dir string) (listed, locked bool, reason, list string) {
	t.Helper()
	list = testGit(t, clone, "worktree", "list", "--porcelain")
	want := resolvedPath(dir)
	for _, block := range strings.Split(list, "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) == 0 || resolvedPath(strings.TrimPrefix(lines[0], "worktree ")) != want {
			continue
		}
		listed = true
		for _, l := range lines[1:] {
			if l == "locked" || strings.HasPrefix(l, "locked ") {
				locked, reason = true, strings.TrimPrefix(strings.TrimPrefix(l, "locked"), " ")
			}
		}
	}
	return listed, locked, reason, list
}

func resolvedPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// A worktree reused by Prepare — a rework, an adopted item — whose lock
// was lifted is locked again, its reason naming the item: its item is
// live again, and a prune must not forget it (ADR-028).
func TestPrepareLocksAReusedUnlockedWorktree(t *testing.T) {
	root, origin := lockTestOrigin(t)
	repos := filepath.Join(root, "repos")
	g, cmds := recordingGit(repos)
	spec := WorkspaceSpec{Root: filepath.Join(root, "work"), GoalSlug: "g", Seq: 1, Repo: origin, Branch: "mesh/g-1", BaseRef: "main"}
	dir, err := g.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	clone := SharedClonePath(repos, origin)
	if listed, locked, _, list := worktreeLock(t, clone, dir); !listed || !locked {
		t.Fatalf("first prepare: listed %v locked %v, want both:\n%s", listed, locked, list)
	}

	testGit(t, clone, "worktree", "unlock", dir)
	if listed, locked, _, list := worktreeLock(t, clone, dir); !listed || locked {
		t.Fatalf("after unlock: listed %v locked %v, want listed and unlocked:\n%s", listed, locked, list)
	}

	before := len(cmds())
	if got, err := g.Prepare(context.Background(), spec); err != nil || got != dir {
		t.Fatalf("second prepare: %v %s", err, got)
	}
	if c := cmds()[before:]; worktreeAdds(c) != 0 {
		t.Fatalf("second prepare added a worktree:\n%s", strings.Join(c, "\n"))
	}
	listed, locked, reason, list := worktreeLock(t, clone, dir)
	if !listed || !locked {
		t.Fatalf("after reuse: listed %v locked %v, want both:\n%s", listed, locked, list)
	}
	if want := "mesh: item g/1 live"; reason != want {
		t.Fatalf("lock reason %q, want %q", reason, want)
	}
}
