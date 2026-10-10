package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRun runs git in dir with a fixed identity, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// cloneAndWorktree makes an origin repo, a full clone of it and a
// worktree of that clone, all under one temp dir, and returns the clone
// and the worktree.
func cloneAndWorktree(t *testing.T) (clone, worktree string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, origin, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, origin, "add", "README")
	gitRun(t, origin, "commit", "-q", "-m", "first")
	clone = filepath.Join(root, "repos", "owner", "name")
	gitRun(t, root, "clone", "-q", origin, clone)
	worktree = filepath.Join(root, "work", "goal", "1")
	gitRun(t, clone, "worktree", "add", "-q", "-b", "item-1", worktree, "main")
	return clone, worktree
}

func TestGitCommonDirOfAWorktreeIsTheClonesGitDir(t *testing.T) {
	clone, worktree := cloneAndWorktree(t)
	want, err := filepath.EvalSymlinks(filepath.Join(clone, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := GitCommonDir(worktree)
	if err != nil {
		t.Fatalf("GitCommonDir(%s): %v", worktree, err)
	}
	t.Logf("GitCommonDir(%s) = %s; EvalSymlinks(%s/.git) = %s", worktree, got, clone, want)
	if got != want {
		t.Fatalf("GitCommonDir(worktree) = %q, want %q", got, want)
	}
}

func TestGitCommonDirOfAFullCloneIsEmpty(t *testing.T) {
	clone, _ := cloneAndWorktree(t)
	got, err := GitCommonDir(clone)
	if err != nil {
		t.Fatalf("GitCommonDir(%s): %v", clone, err)
	}
	t.Logf("GitCommonDir(%s) = %q", clone, got)
	if got != "" {
		t.Fatalf("GitCommonDir(full clone) = %q, want \"\"", got)
	}
}

func TestGitCommonDirOfARelativeWorktreeIsAbsolute(t *testing.T) {
	clone, worktree := cloneAndWorktree(t)
	want, err := filepath.EvalSymlinks(filepath.Join(clone, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(worktree))
	rel := filepath.Join(".", filepath.Base(worktree))
	got, err := GitCommonDir(rel)
	if err != nil {
		t.Fatalf("GitCommonDir(%s): %v", rel, err)
	}
	t.Logf("GitCommonDir(%s) = %s", rel, got)
	if !filepath.IsAbs(got) {
		t.Fatalf("GitCommonDir(%q) = %q, not absolute", rel, got)
	}
	if got != want {
		t.Fatalf("GitCommonDir(%q) = %q, want %q", rel, got, want)
	}
}

func TestGitCommonDirOfNoCheckoutIsAnError(t *testing.T) {
	if _, err := GitCommonDir(t.TempDir()); err == nil {
		t.Fatal("GitCommonDir(empty dir) = nil error, want one")
	}
}
