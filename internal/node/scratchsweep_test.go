package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// scratchDir makes dir with one file of size bytes in it.
func scratchDir(t *testing.T, dir string, size int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func lstatOK(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// A node killed mid-session left its scratch: the start-up sweep removes
// the stale run scratch and the node's own stale /tmp dirs, keeps what an
// adopted session's state names and what a live run holds, leaves a
// non-mesh prefix and another user's dir alone, and logs the counts.
func TestStartUpScratchSweep(t *testing.T) {
	tmp, root := t.TempDir(), filepath.Join(t.TempDir(), ".scratch")

	staleRun := scratchDir(t, filepath.Join(root, "validate-item-1-aaaa"), 10)
	staleBuild := scratchDir(t, filepath.Join(root, "build-item-2-bbbb"), 20)
	keptTMP := scratchDir(t, filepath.Join(root, "build-item-3-cccc"), 5)
	// a no-workspace session's dir under its TMPDIR: kept with it
	keptSessionDir := scratchDir(t, filepath.Join(keptTMP, "mesh-session-x"), 1)
	liveRun := scratchDir(t, filepath.Join(root, "served-batch-dddd"), 5)

	staleSession := scratchDir(t, filepath.Join(tmp, "mesh-session-plan-g-1234"), 1)
	staleGH := scratchDir(t, filepath.Join(tmp, "mesh-gh-111"), 2)
	staleClaude := scratchDir(t, filepath.Join(tmp, "mesh-claude-222"), 3)
	staleHome := scratchDir(t, filepath.Join(tmp, "mesh-home-333"), 4)
	keptGH := scratchDir(t, filepath.Join(tmp, "mesh-gh-kept"), 7)
	keptClaude := scratchDir(t, filepath.Join(tmp, "mesh-claude-kept"), 7)
	foreign := scratchDir(t, filepath.Join(tmp, "runsheet-123"), 9)
	unmeshed := scratchDir(t, filepath.Join(tmp, "go-build999"), 9)
	otherUser := scratchDir(t, filepath.Join(tmp, "mesh-gh-otheruser"), 9)
	otherUserScratch := scratchDir(t, filepath.Join(root, "build-other-user"), 9)
	// a symlink with a mesh prefix is never followed or removed
	target := scratchDir(t, filepath.Join(t.TempDir(), "target"), 9)
	link := filepath.Join(tmp, "mesh-session-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	found := []FoundSession{{
		Dir: keptSessionDir,
		State: sessionunit.State{
			Kind:     "plan",
			TempDirs: []string{keptClaude, keptGH, keptTMP},
		},
	}}
	var lines []string
	res := SweepScratch(ScratchSweep{
		Roots:   []string{root},
		TempDir: tmp,
		Found:   found,
		InUse:   func(p string) bool { return p == liveRun },
		Owned: func(info os.FileInfo) bool {
			return !strings.Contains(info.Name(), "other")
		},
	}, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })

	for _, gone := range []string{staleRun, staleBuild, staleSession, staleGH, staleClaude, staleHome} {
		if lstatOK(gone) {
			t.Errorf("%s survived the sweep", gone)
		}
	}
	for _, kept := range []string{keptTMP, keptSessionDir, liveRun, keptGH, keptClaude, foreign, unmeshed, otherUser, otherUserScratch, link, target} {
		if !lstatOK(kept) {
			t.Errorf("%s was swept", kept)
		}
	}
	if res.Dirs != 6 || res.Bytes != 10+20+1+2+3+4 || res.Kept != 4 {
		t.Errorf("result = %+v, want 6 dirs, 40 bytes, 4 kept", res)
	}
	want := "scratch: swept 6 dirs, 40 bytes at start (4 kept)"
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("logged %q, want [%q]", lines, want)
	}
}

// On a sandboxed node mesh-home-* is the sandbox sweep's: the scratch
// sweep leaves it.
func TestStartUpScratchSweepSandboxedLeavesHomes(t *testing.T) {
	tmp := t.TempDir()
	home := scratchDir(t, filepath.Join(tmp, "mesh-home-1"), 1)
	gh := scratchDir(t, filepath.Join(tmp, "mesh-gh-1"), 1)
	res, err := ScratchSweep{TempDir: tmp, Sandboxed: true}.Run()
	if err != nil {
		t.Fatal(err)
	}
	if !lstatOK(home) || lstatOK(gh) || res.Dirs != 1 {
		t.Errorf("home kept %v, gh gone %v, result %+v", lstatOK(home), !lstatOK(gh), res)
	}
}

// A scratch root that was never made is nothing to sweep, not an error.
func TestStartUpScratchSweepMissingRoot(t *testing.T) {
	res, err := ScratchSweep{Roots: []string{filepath.Join(t.TempDir(), "none")}, TempDir: t.TempDir()}.Run()
	if err != nil || res != (ScratchSweepResult{}) {
		t.Errorf("Run = %+v, %v", res, err)
	}
}

// A run's scratch holding a read-only Go module cache is still removed.
func TestStartUpScratchSweepReadOnlyTree(t *testing.T) {
	root := t.TempDir()
	dir := scratchDir(t, filepath.Join(root, "validate-x"), 1)
	ro := scratchDir(t, filepath.Join(dir, "gopath", "pkg", "mod", "m@v1"), 1)
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	res, err := ScratchSweep{Roots: []string{root}, TempDir: t.TempDir()}.Run()
	if err != nil || lstatOK(dir) || res.Dirs != 1 {
		t.Errorf("Run = %+v, %v; dir left %v", res, err, lstatOK(dir))
	}
}

// The process's own HOME is never swept, though it is a mesh-home-* dir
// under the swept /tmp (a pod's HOME mounted at its host path): a test or
// session running `mesh node start` there keeps its HOME.
func TestStartUpScratchSweepKeepsTheProcessHome(t *testing.T) {
	tmp := t.TempDir()
	home := scratchDir(t, filepath.Join(tmp, "mesh-home-42"), 4)
	stale := scratchDir(t, filepath.Join(tmp, "mesh-home-43"), 4)
	t.Setenv("HOME", home)
	res, err := ScratchSweep{TempDir: tmp, Owned: func(os.FileInfo) bool { return true }}.Run()
	if err != nil {
		t.Fatal(err)
	}
	if !lstatOK(home) || lstatOK(stale) || res.Dirs != 1 || res.Kept != 1 {
		t.Fatalf("home kept %v, stale kept %v, %+v", lstatOK(home), lstatOK(stale), res)
	}
}
