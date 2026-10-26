//go:build e2e

package node

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// A sandboxed session cannot prune the shared clone's worktrees
// (ADR-034): two items of one repo are two worktrees of one clone, as
// checkouts were before each was a clone of its own (prepareWorktree); item
// 1's session pod mounts the clone's common dir, `.git/worktrees`
// read-only over it and item 1's own entry read-write over that. Item 2's
// work tree is absent in the pod, so its entry's gitdir pointer dangles
// there — `git worktree prune` and `git worktree prune --expire now` in
// the pod would delete it on the host if they could write. After both,
// both entries are still in `<clone>/.git/worktrees`, item 2's checkout
// still answers `git status`, and the commit item 1's session made in its
// own worktree landed. The mount list is checked first, without podman;
// the pod needs podman and an image with git (MESH_SANDBOX_IMAGE, else
// mesh-node at dev), and is skipped without either.
func TestPodCannotPruneWorktrees(t *testing.T) {
	root, origin := lockTestOrigin(t)
	work, repos := filepath.Join(root, "work"), filepath.Join(root, "repos")
	g, _ := recordingGit(repos)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var dirs, entries []string
	for seq := 1; seq <= 2; seq++ {
		spec := WorkspaceSpec{Root: work, GoalSlug: "g", Seq: seq, Repo: origin, Branch: BranchName("g", seq), BaseRef: "main"}
		dir, err := prepareWorktree(ctx, g, spec)
		if err != nil {
			t.Fatalf("prepare seq %d: %v", seq, err)
		}
		if !isWorktree(dir) {
			t.Fatalf("seq %d: %s is not a worktree", seq, dir)
		}
		entry, err := sandbox.GitDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		dirs, entries = append(dirs, dir), append(entries, entry)
	}
	common, err := sandbox.GitCommonDir(dirs[0])
	if err != nil || common == "" {
		t.Fatalf("common dir of %s: %q %v", dirs[0], common, err)
	}
	wts := filepath.Join(common, "worktrees")
	for i, e := range entries {
		if filepath.Dir(e) != wts {
			t.Fatalf("seq %d's entry %s is not under %s", i+1, e, wts)
		}
	}
	t.Logf("shared clone %s; entries before: %v", common, listEntries(t, wts))

	// the session's mounts: .git/worktrees read-only, writable only at
	// item 1's own entry
	image := os.Getenv("MESH_SANDBOX_IMAGE")
	if image == "" {
		image = sandbox.DefaultImage("dev")
	}
	rt := &sandbox.Podman{}
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{Image: image}.Settings("dev"), TempDir: t.TempDir(), Logf: t.Logf}
	sess, err := sb.Open("e2e-prune-g-1", "build", sandbox.GitIdentity{Name: "Mesh builder (e2e)", Email: "mesh-builder+e2e@runsheet.dev"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	roWorktrees := false
	for _, m := range sess.Mounts(dirs[0]) {
		t.Logf("mount %s:%s ro=%v", m.Source, m.Target, m.ReadOnly)
		target := filepath.Clean(m.Target)
		if target == wts && m.ReadOnly {
			roWorktrees = true
		}
		under := target == wts || strings.HasPrefix(target, wts+string(filepath.Separator))
		if under && !m.ReadOnly && target != entries[0] {
			t.Errorf("read-write mount under %s other than the session's own entry %s: %+v", wts, entries[0], m)
		}
	}
	if !roWorktrees {
		t.Fatalf("no read-only mount of %s", wts)
	}

	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("no podman: the in-pod prune needs real pods")
	}
	if _, err := rt.Version(ctx); err != nil {
		t.Skipf("podman: %v", err)
	}
	if out, err := exec.Command("podman", "image", "exists", image).CombinedOutput(); err != nil {
		t.Skipf("image %s is not here (MESH_SANDBOX_IMAGE names another with git): %v %s", image, err, out)
	}

	// in item 1's pod: both prunes, then a commit in its own worktree
	script := `set -u
echo "worktrees in the pod: $(ls ` + shellWord(wts) + ` | tr '\n' ' ')"
git worktree prune -v; echo "git worktree prune exit $?"
git worktree prune -v --expire now; echo "git worktree prune --expire now exit $?"
echo pod > pod-commit
git add pod-commit && git commit -q -m "commit from the pod" && echo "commit exit 0"
`
	var stdout, stderr bytes.Buffer
	runErr := sess.Run(ctx, sandbox.RunSpec{
		Workspace: dirs[0],
		Command:   []string{"sh", "-c", script},
		Timeout:   2 * time.Minute,
		Stdout:    &stdout,
		Stderr:    &stderr,
	})
	t.Logf("pod stdout:\n%s", stdout.String())
	t.Logf("pod stderr:\n%s", stderr.String())
	if runErr != nil {
		t.Fatalf("the pod's script: %v", runErr)
	}
	if !strings.Contains(stdout.String(), "commit exit 0") {
		t.Fatalf("the commit in the session's own worktree did not succeed")
	}

	// on the host: both entries, item 2 still a checkout, item 1's commit
	after := listEntries(t, wts)
	t.Logf("entries after the in-pod prunes: %v", after)
	for i, e := range entries {
		if _, err := os.Stat(filepath.Join(e, "gitdir")); err != nil {
			t.Errorf("seq %d's entry %s after the in-pod prunes: %v", i+1, e, err)
		}
	}
	if len(after) != 2 {
		t.Errorf("%d entries in %s after the in-pod prunes, want 2: %v", len(after), wts, after)
	}
	status := testGit(t, dirs[1], "status", "--short", "--branch")
	t.Logf("git -C %s status: %s", dirs[1], status)
	if subject := testGit(t, dirs[0], "log", "-1", "--format=%s"); subject != "commit from the pod" {
		t.Errorf("item 1's head after the pod: %q, want the pod's commit", subject)
	}
	list := testGit(t, filepath.Dir(common), "worktree", "list")
	t.Logf("git worktree list:\n%s", list)
	for _, d := range dirs {
		if !strings.Contains(list, d) {
			t.Errorf("the shared clone no longer lists %s", d)
		}
	}
}

// listEntries is the sorted names in a clone's .git/worktrees.
func listEntries(t *testing.T, wts string) []string {
	t.Helper()
	des, err := os.ReadDir(wts)
	if err != nil {
		t.Fatalf("list %s: %v", wts, err)
	}
	var names []string
	for _, d := range des {
		names = append(names, d.Name())
	}
	sort.Strings(names)
	return names
}

// shellWord single-quotes s for sh.
func shellWord(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
