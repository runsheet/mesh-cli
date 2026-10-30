package githardentest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"projectmesh.io/mesh-cli/internal/githarden"
)

// checkout is a checkout of two commits; it answers it and the first.
func checkout(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	Git(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-qm", "one")
	first := Git(t, dir, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644)
	Git(t, dir, "commit", "-qam", "two")
	return dir, first
}

// plantedRuns are git commands, each reaching one kind of planted
// command when git honours the checkout's config, and the marker it
// leaves.
func plantedRuns(first string) []struct {
	marker string
	args   []string
} {
	return []struct {
		marker string
		args   []string
	}{
		{"fsmonitor", []string{"status", "--porcelain"}},
		{"hookspath-post-checkout", []string{"checkout", "-q", "--detach", first}},
		{"credential", []string{"fetch", "-q", "ASKING"}},
		{"sshcommand", []string{"fetch", "-q", SSHRemote}},
	}
}

// The plant is real: git that honours the checkout's config runs each
// planted command — the fsmonitor on status, the hooks path's and
// .git/hooks' post-checkout on checkout, the credential helper on a
// remote that asks, the ssh command on an ssh remote. A test that finds
// none ran under the node's git therefore proves something.
func TestThePlantRunsUnhardened(t *testing.T) {
	dir, first := checkout(t)
	p := New(t, dir)
	asking := AskingServer(t)
	for _, run := range plantedRuns(first) {
		args := slices.Clone(run.args)
		if i := slices.Index(args, "ASKING"); i >= 0 {
			args[i] = asking
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
		cmd.Run()
		if !slices.Contains(p.Ran(), run.marker) {
			t.Errorf("git %v did not run the planted %s: %v", run.args, run.marker, p.Ran())
		}
	}
	gitDir := Git(t, dir, "rev-parse", "--absolute-git-dir")
	Git(t, dir, "-c", "core.hooksPath="+filepath.Join(gitDir, "hooks"), "checkout", "-q", "--detach", "HEAD")
	if !slices.Contains(p.Ran(), "hook-post-checkout") {
		t.Errorf("checkout did not run .git/hooks/post-checkout: %v", p.Ran())
	}
}

// The hardened git runs none of it, through the same commands.
func TestHardenedGitRunsNoneOfThePlant(t *testing.T) {
	dir, first := checkout(t)
	p := New(t, dir)
	asking := AskingServer(t)
	for _, run := range plantedRuns(first) {
		args := slices.Clone(run.args)
		if i := slices.Index(args, "ASKING"); i >= 0 {
			args[i] = asking
		}
		githarden.Command(context.Background(), dir, nil, args...).Run()
	}
	p.NoneRan(t, "githarden.Command")
}
