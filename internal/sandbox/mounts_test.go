package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// newFullClone makes an origin repo and a full clone of it with `git
// clone` — its `.git` a directory, the layout from before mesh-specs ADR
// 017 — and returns the clone.
func newFullClone(t *testing.T) string {
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
	clone := filepath.Join(root, "work", "goal", "1")
	gitRun(t, root, "clone", "-q", origin, clone)
	if info, err := os.Stat(filepath.Join(clone, ".git")); err != nil || !info.IsDir() {
		t.Fatalf("%s/.git is not a directory: %v", clone, err)
	}
	return clone
}

func openSession(t *testing.T, hooks string) *Session {
	t.Helper()
	sb := testSandbox(t, &Fake{})
	sb.Logf = t.Logf
	sess, err := sb.Open("ship-it/2", "build", GitIdentity{HooksDir: hooks})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// A worktree's pod mounts the clone's git common dir read-write at its
// own path, and nothing else under the clone's parent: not the clone's
// work tree, not another item's worktree.
func TestMountsOfAWorktreeAddTheCommonDir(t *testing.T) {
	clone, worktree := cloneAndWorktree(t)
	other := filepath.Join(filepath.Dir(worktree), "2")
	gitRun(t, clone, "worktree", "add", "-q", "-b", "item-2", other, "main")
	common, err := filepath.EvalSymlinks(filepath.Join(clone, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(clone))
	if err != nil {
		t.Fatal(err)
	}
	sess := openSession(t, "")
	got := sess.Mounts(worktree)
	t.Logf("Mounts(%s) = %+v", worktree, got)
	if !slices.Contains(got, Mount{Source: common, Target: common, ReadOnly: false}) {
		t.Fatalf("mounts = %+v, want %s read-write", got, common)
	}
	under := 0
	for _, m := range got {
		src, err := filepath.EvalSymlinks(m.Source)
		if err != nil {
			t.Fatal(err)
		}
		if src == parent || strings.HasPrefix(src, parent+string(filepath.Separator)) {
			under++
			if src != common {
				t.Fatalf("mounted %s under the clone's parent %s", src, parent)
			}
		}
		if src == other {
			t.Fatalf("mounted another item's worktree %s", other)
		}
	}
	if under != 1 {
		t.Fatalf("%d mounts under %s, want the common dir alone", under, parent)
	}
	want := []Mount{{Source: worktree, Target: worktree}, {Source: sess.Home, Target: sess.Home}, {Source: common, Target: common}}
	if !slices.Equal(got, want) {
		t.Fatalf("mounts = %+v, want %+v", got, want)
	}
}

// A full clone mounts exactly what it did before the layout: the
// workspace and HOME read-write.
func TestMountsOfAFullCloneWithoutHooks(t *testing.T) {
	ws := newFullClone(t)
	sess := openSession(t, "")
	got := sess.Mounts(ws)
	want := []Mount{{Source: ws, Target: ws}, {Source: sess.Home, Target: sess.Home}}
	t.Logf("Mounts(%s) = %+v", ws, got)
	if !slices.Equal(got, want) {
		t.Fatalf("mounts = %+v, want %+v", got, want)
	}
}

// A full clone with hooks set adds only the hooks, read-only.
func TestMountsOfAFullCloneWithHooks(t *testing.T) {
	ws := newFullClone(t)
	hooks := t.TempDir()
	sess := openSession(t, hooks)
	got := sess.Mounts(ws)
	want := []Mount{{Source: ws, Target: ws}, {Source: sess.Home, Target: sess.Home}, {Source: hooks, Target: hooks, ReadOnly: true}}
	t.Logf("Mounts(%s) = %+v", ws, got)
	if !slices.Equal(got, want) {
		t.Fatalf("mounts = %+v, want %+v", got, want)
	}
}

// The podman run of a worktree workspace passes the common dir as a
// --volume at its own path.
func TestPodmanArgsOfAWorktreeMountTheCommonDir(t *testing.T) {
	clone, worktree := cloneAndWorktree(t)
	common, err := filepath.EvalSymlinks(filepath.Join(clone, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	f := &Fake{}
	sb := testSandbox(t, f)
	sb.Logf = t.Logf
	sess, err := sb.Open("ship-it/2", "build", GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Run(context.Background(), RunSpec{Workspace: worktree, Command: []string{"claude", "-p"}, Stdin: strings.NewReader("prompt")}); err != nil {
		t.Fatal(err)
	}
	if len(f.Agents) != 1 {
		t.Fatalf("agents: %+v", f.Agents)
	}
	args := AgentRunArgs(Pod{Name: "p"}, f.Agents[0])
	var vols []string
	for i, a := range args {
		if a == "--volume" {
			vols = append(vols, args[i+1])
		}
	}
	t.Logf("volumes = %v", vols)
	if !slices.Contains(vols, common+":"+common+":rw") {
		t.Fatalf("volumes = %v, want --volume %s:%s:rw", vols, common, common)
	}
	want := []string{worktree + ":" + worktree + ":rw", sess.Home + ":" + sess.Home + ":rw", common + ":" + common + ":rw"}
	if !slices.Equal(vols, want) {
		t.Fatalf("volumes = %v, want %v", vols, want)
	}
}
