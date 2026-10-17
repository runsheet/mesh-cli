package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// touchTree sets every file and directory under root to mtime when,
// so a dir's "newest mtime anywhere inside" reads as when.
func touchTree(t *testing.T, root string, when time.Time) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, when, when)
	}); err != nil {
		t.Fatalf("touchTree(%s): %v", root, err)
	}
}

// writeFile makes a file of n bytes under dir, creating dir first.
func writeNBytes(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A runsheet-* dir older than the minimum age is removed, and its
// bytes land in Reclaimed.
func TestScratchCheckReapsAnOldMatchingDir(t *testing.T) {
	tmp := t.TempDir()
	old := filepath.Join(tmp, "runsheet-abc123")
	writeNBytes(t, filepath.Join(old, "file.bin"), 500)
	now := time.Now()
	touchTree(t, old, now.Add(-2*time.Hour))

	c := &ScratchCheck{TempDir: tmp, MinAge: time.Hour}
	got := c.Check(context.Background(), now)
	if len(got) != 1 {
		t.Fatalf("Check() = %+v, want 1 finding", got)
	}
	if got[0].Kind != "scratch" {
		t.Fatalf("Kind = %q, want scratch", got[0].Kind)
	}
	if got[0].Reclaimed != 500 {
		t.Fatalf("Reclaimed = %d, want 500", got[0].Reclaimed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("%s still there: %v", old, err)
	}
}

// A fresh matching dir, one InUse names, a non-matching name and a
// symlink to a dir outside are all kept.
func TestScratchCheckKeepsWhatItShould(t *testing.T) {
	tmp := t.TempDir()
	now := time.Now()

	fresh := filepath.Join(tmp, "runsheet-fresh")
	writeNBytes(t, filepath.Join(fresh, "f"), 10)
	// left at its just-created mtime: younger than MinAge.

	held := filepath.Join(tmp, "runsheet-held")
	writeNBytes(t, filepath.Join(held, "f"), 20)
	touchTree(t, held, now.Add(-2*time.Hour))

	other := filepath.Join(tmp, "not-ours")
	writeNBytes(t, filepath.Join(other, "f"), 30)
	touchTree(t, other, now.Add(-2*time.Hour))

	outside := t.TempDir()
	target := filepath.Join(outside, "target")
	writeNBytes(t, filepath.Join(target, "f"), 40)
	touchTree(t, target, now.Add(-2*time.Hour))
	link := filepath.Join(tmp, "runsheet-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	c := &ScratchCheck{
		TempDir: tmp,
		MinAge:  time.Hour,
		InUse:   func(path string) bool { return path == held },
	}
	got := c.Check(context.Background(), now)
	if len(got) != 1 {
		t.Fatalf("Check() = %+v, want 1 finding", got)
	}
	if got[0].Reclaimed != 0 {
		t.Fatalf("Reclaimed = %d, want 0 (nothing eligible to remove)", got[0].Reclaimed)
	}
	if got[0].Bytes != 20 {
		t.Fatalf("Bytes (left over) = %d, want 20 (the held dir)", got[0].Bytes)
	}
	for _, p := range []string{fresh, held, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s should still be there: %v", p, err)
		}
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the symlink should still be there: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the symlink's target should be untouched: %v", err)
	}
}

// A read-only nested directory (as a Go module cache leaves one) is
// still removed: ScratchCheck chmods u+w onto it first.
func TestScratchCheckRemovesAReadOnlyNestedDir(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "go-build1234")
	nested := filepath.Join(root, "pkg", "mod", "example.com@v1.0.0")
	writeNBytes(t, filepath.Join(nested, "file.go"), 7)
	now := time.Now()
	touchTree(t, root, now.Add(-2*time.Hour))
	if err := os.Chmod(nested, 0o500); err != nil {
		t.Fatal(err)
	}

	c := &ScratchCheck{TempDir: tmp, MinAge: time.Hour}
	got := c.Check(context.Background(), now)
	if len(got) != 1 || got[0].Reclaimed != 7 {
		t.Fatalf("Check() = %+v, want one finding reclaiming 7 bytes", got)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("%s still there: %v", root, err)
	}
}

// With a fake sandbox runtime, the periodic sweep removes a stale
// mesh-home dir and keeps one a running container still mounts — the
// same rule node.SweepSandbox applies at start, now run on every tick.
func TestScratchCheckRunsTheSandboxSweepPeriodically(t *testing.T) {
	tmp := t.TempDir()
	rt := &sandbox.Fake{}
	sb := &sandbox.Sandbox{Runtime: rt, TempDir: tmp}

	live, err := sb.Open("item 3", "build", sandbox.GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := sb.Open("item 2", "build", sandbox.GitIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	rt.Running = []sandbox.Container{
		{Name: "mesh-live-0-agent", Pod: "mesh-live-0", Running: true, Mounts: []string{live.Home}},
		{Name: "mesh-live-0-infra", Pod: "mesh-live-0", Running: true, Infra: true},
		{Name: "mesh-stale-0-agent", Pod: "mesh-stale-0"},
		{Name: "mesh-stale-0-infra", Pod: "mesh-stale-0", Running: true, Infra: true},
	}
	rt.Standing = []sandbox.PodInfo{
		{Name: "mesh-live-0", Labels: map[string]string{sandbox.SessionLabel: "item 3"}},
		{Name: "mesh-stale-0", Labels: map[string]string{sandbox.SessionLabel: "item 2"}},
	}

	c := &ScratchCheck{TempDir: tmp, MinAge: time.Hour, Sandbox: sb}
	got := c.Check(context.Background(), time.Now())
	if len(got) != 1 {
		t.Fatalf("Check() = %+v, want 1 finding", got)
	}

	if _, err := os.Lstat(stale.Home); !os.IsNotExist(err) {
		t.Fatalf("the stale HOME %s is still there", stale.Home)
	}
	if _, err := os.Lstat(live.Home); err != nil {
		t.Fatalf("the running session's HOME went: %v", err)
	}
}

// The finding warns when the leftover (bytes kept past the minimum
// age) exceeds the threshold, and is ok otherwise.
func TestScratchCheckWarnsOverTheThreshold(t *testing.T) {
	now := time.Now()
	newHeld := func(t *testing.T, n int) (tmp, held string) {
		tmp = t.TempDir()
		held = filepath.Join(tmp, "runsheet-held")
		writeNBytes(t, filepath.Join(held, "f"), n)
		touchTree(t, held, now.Add(-2*time.Hour))
		return
	}

	t.Run("warn", func(t *testing.T) {
		tmp, held := newHeld(t, 100)
		c := &ScratchCheck{TempDir: tmp, MinAge: time.Hour, WarnBytes: 50, InUse: func(p string) bool { return p == held }}
		got := c.Check(context.Background(), now)
		if len(got) != 1 || got[0].Severity != "warn" {
			t.Fatalf("Check() = %+v, want 1 warn finding", got)
		}
	})

	t.Run("ok", func(t *testing.T) {
		tmp, held := newHeld(t, 10)
		c := &ScratchCheck{TempDir: tmp, MinAge: time.Hour, WarnBytes: 50, InUse: func(p string) bool { return p == held }}
		got := c.Check(context.Background(), now)
		if len(got) != 1 || got[0].Severity != "ok" {
			t.Fatalf("Check() = %+v, want 1 ok finding", got)
		}
	})
}
