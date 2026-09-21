package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Prepare on a clone whose branch exists locally but not on origin — an
// attempt interrupted before its push — continues that branch instead
// of resetting it onto the base.
func TestPrepareKeepsAnUnpushedLocalBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(root, "init", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	git(root, "clone", "-q", origin, seed)
	os.WriteFile(filepath.Join(seed, "a"), []byte("a"), 0o644)
	git(seed, "add", "a")
	git(seed, "commit", "-q", "-m", "a")
	git(seed, "push", "-q", "origin", "HEAD:main")

	// the workspace: a clone with an unpushed commit on the item's branch
	spec := WorkspaceSpec{Root: root, GoalSlug: "g", Seq: 1, Repo: origin, Branch: "mesh/g-1", BaseRef: "main"}
	dir := spec.Dir()
	git(root, "clone", "-q", origin, dir)
	git(dir, "checkout", "-q", "-b", "mesh/g-1")
	os.WriteFile(filepath.Join(dir, "b"), []byte("b"), 0o644)
	git(dir, "add", "b")
	git(dir, "commit", "-q", "-m", "b")
	local := git(dir, "rev-parse", "HEAD")

	g := &Git{}
	got, err := g.Prepare(context.Background(), spec)
	if err != nil || got != dir {
		t.Fatalf("prepare: %v %s", err, got)
	}
	if head := git(dir, "rev-parse", "HEAD"); head != local {
		t.Fatalf("the local branch was reset: %s != %s", head, local)
	}
	if branch := git(dir, "branch", "--show-current"); branch != "mesh/g-1" {
		t.Fatalf("branch = %s", branch)
	}
}
