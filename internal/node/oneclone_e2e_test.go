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

// Two items of one repo on a node open one shared clone, which seeds a
// clone of its own for each, and both build and push (ADR-028, ADR-039):
// against a local bare origin, with gh's clone substituted by git's
// (lockTestOrigin), the node's Git prepares both items — the shared
// clone's `gh repo clone`, then one seeded `gh repo clone` per item and no
// `git worktree add` — each commits a change on its own branch and pushes
// it, both branches are on the origin, and RemoveWorkspace on both leaves
// the shared clone.
func TestOneCloneSeedsTwoCheckoutsBuildAndPushE2E(t *testing.T) {
	root, origin := lockTestOrigin(t)
	work, repos := filepath.Join(root, "work"), filepath.Join(root, "repos")
	g, cmds := recordingGit(repos)
	ctx := context.Background()
	clone := SharedClonePath(repos, origin)

	var dirs []string
	var branches []string
	for seq := 1; seq <= 2; seq++ {
		spec := WorkspaceSpec{Root: work, GoalSlug: "g", Seq: seq, Repo: origin, Branch: BranchName("g", seq), BaseRef: "main"}
		dir, err := g.Prepare(ctx, spec)
		if err != nil {
			t.Fatalf("prepare seq %d: %v", seq, err)
		}
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
			t.Fatalf("seq %d: %s is not a clone of its own: %v", seq, dir, err)
		}
		// the item's build: one change, committed on its branch
		name := "item-" + spec.Branch[strings.LastIndex(spec.Branch, "-")+1:]
		if err := os.WriteFile(filepath.Join(dir, name), []byte(spec.Branch+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		testGit(t, dir, "add", name)
		testGit(t, dir, "commit", "-q", "-m", "build "+spec.Branch)
		head := testGit(t, dir, "rev-parse", "HEAD")
		pushed, err := g.Push(ctx, dir, spec.Branch)
		if err != nil {
			t.Fatalf("push seq %d: %v", seq, err)
		}
		if pushed != head {
			t.Fatalf("seq %d pushed %s, committed %s", seq, pushed, head)
		}
		t.Logf("seq %d: clone %s on %s, pushed %s", seq, dir, spec.Branch, pushed)
		dirs = append(dirs, dir)
		branches = append(branches, spec.Branch)
	}

	clones, adds := 0, 0
	for _, c := range cmds() {
		if strings.HasPrefix(c, "gh repo clone ") {
			clones++
			t.Logf("$ %s", c)
		}
		if strings.HasPrefix(c, "git worktree add ") {
			adds++
			t.Logf("$ %s", c)
		}
	}
	if clones != 3 || adds != 0 {
		t.Fatalf("%d gh repo clone(s) and %d git worktree add(s), want 3 and 0:\n%s", clones, adds, strings.Join(cmds(), "\n"))
	}

	remote, err := exec.Command("git", "ls-remote", "--heads", origin).Output()
	if err != nil {
		t.Fatalf("git ls-remote %s: %v", origin, err)
	}
	t.Logf("git ls-remote --heads origin:\n%s", strings.TrimSpace(string(remote)))
	for i, b := range branches {
		head := testGit(t, dirs[i], "rev-parse", "HEAD")
		if !strings.Contains(string(remote), head+"\trefs/heads/"+b) {
			t.Fatalf("the origin lacks %s at %s:\n%s", b, head, remote)
		}
	}

	for _, dir := range dirs {
		if err := g.RemoveWorkspace(ctx, dir); err != nil {
			t.Fatalf("remove %s: %v", dir, err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s is still there after its removal: %v", dir, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(clone, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("the shared clone %s after both removals: %v", clone, err)
	}
	list := testGit(t, clone, "worktree", "list")
	if strings.Contains(list, dirs[0]) || strings.Contains(list, dirs[1]) {
		t.Fatalf("the shared clone still registers a removed worktree:\n%s", list)
	}
	t.Logf("shared clone %s still present; worktree list:\n%s", clone, list)
}
