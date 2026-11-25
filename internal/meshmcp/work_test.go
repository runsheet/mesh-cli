package meshmcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cloneWithDefault makes a bare origin whose HEAD is def (its "default
// branch") with one commit, and a clone of it at dir.
func cloneWithDefault(t *testing.T, root, def string) (dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	for _, kv := range []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	origin := filepath.Join(root, def+"-origin.git")
	gitIn(t, root, "init", "--quiet", "--bare", "-b", def, origin)
	seed := filepath.Join(root, def+"-seed")
	gitIn(t, root, "clone", "--quiet", origin, seed)
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", "f.txt")
	gitIn(t, seed, "commit", "--quiet", "-m", "base")
	gitIn(t, seed, "push", "--quiet", "origin", "HEAD:"+def)
	dir = filepath.Join(root, def+"-work")
	gitIn(t, root, "clone", "--quiet", origin, dir)
	return dir
}

// baseOf, with no base resolved at join time (an item whose base_ref and
// repo default_branch were both unknown when it was joined), reads the
// checkout's own origin/HEAD — never a guessed "main": a repo whose
// actual default is master names master, one whose default really is
// main still names main, unchanged.
func TestDefaultBranchReadsOriginHEADNotMain(t *testing.T) {
	root := t.TempDir()
	for _, def := range []string{"master", "main"} {
		t.Run(def, func(t *testing.T) {
			dir := cloneWithDefault(t, root, def)
			h := HeldItem{Path: dir}
			if got := baseOf(context.Background(), h); got != def {
				t.Fatalf("baseOf = %q, want %q", got, def)
			}
			commit(t, dir, "g.txt", "y\n")
			n, err := commitsPastBase(context.Background(), h)
			if err != nil {
				t.Fatalf("commitsPastBase: %v", err)
			}
			if n != 1 {
				t.Fatalf("commitsPastBase = %d, want 1", n)
			}
		})
	}
}
