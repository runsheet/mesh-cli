// Tests scripts/stale-worktrees.sh on a fixture work root: a checkout
// whose .git file names a gitdir that is gone is printed, one whose
// gitdir is there, a full clone's .git dir and a relative gitdir that
// resolves are not, and a missing root prints nothing and exits 0
// (ADR-020, a reading, never a verdict).
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaleWorktreesListsADanglingGitFile(t *testing.T) {
	root := t.TempDir()
	admin := filepath.Join(root, "repo", ".git", "worktrees", "2")
	for _, d := range []string{"g/1", "g/2", "g/3/.git", "g/4/admin", "repo/.git/worktrees/2"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gone := filepath.Join(root, "repo", ".git", "worktrees", "1")
	files := map[string]string{
		"g/1/.git": "gitdir: " + gone + "\n",
		"g/2/.git": "gitdir: " + admin + "\n",
		"g/4/.git": "gitdir: admin\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command("bash", "stale-worktrees.sh", root).CombinedOutput()
	if err != nil {
		t.Fatalf("stale-worktrees.sh: %v\n%s", err, out)
	}
	if got, want := string(out), filepath.Join(root, "g", "1")+" -> "+gone+"\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}

	out, err = exec.Command("bash", "stale-worktrees.sh", filepath.Join(root, "none")).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("a missing root: %v %q, want nothing and exit 0", err, out)
	}
}
