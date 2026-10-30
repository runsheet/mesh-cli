package gitrelay

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/githarden/githardentest"
)

// The relay proves a fast-forward in the session's own checkout, so a
// hook, fsmonitor, hooks path, credential helper or ssh command the
// session planted there runs nothing on the host, and the answer is
// still git's (mesh-specs gap 7).
func TestCheckoutAncestryRunsNothingTheSessionPlanted(t *testing.T) {
	dir := t.TempDir()
	githardentest.Git(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	githardentest.Git(t, dir, "add", "-A")
	githardentest.Git(t, dir, "commit", "-qm", "one")
	old := githardentest.Git(t, dir, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644)
	githardentest.Git(t, dir, "commit", "-qam", "two")
	new := githardentest.Git(t, dir, "rev-parse", "HEAD")
	p := githardentest.New(t, dir)
	ctx := context.Background()
	if err := CheckoutAncestry(dir)(ctx, old, new); err != nil {
		t.Fatalf("a fast-forward refused: %v", err)
	}
	if err := CheckoutAncestry(dir)(ctx, new, old); err == nil {
		t.Fatal("a force push proved a fast-forward")
	}
	p.NoneRan(t, "the relay's fast-forward check")
}
