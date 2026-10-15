package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBlobSource serves trees and blobs from maps, counting each call;
// gate, when set, holds every Blob call until it is closed.
type fakeBlobSource struct {
	trees     map[string]CacheTree
	blobs     map[string][]byte
	gate      chan struct{}
	treeCalls atomic.Int64
	blobCalls atomic.Int64
	mu        sync.Mutex
	asked     []string
}

func (f *fakeBlobSource) Tree(_ context.Context, _, commit string) (CacheTree, error) {
	f.treeCalls.Add(1)
	t, ok := f.trees[commit]
	if !ok {
		return CacheTree{}, fmt.Errorf("no commit %s", commit)
	}
	return t, nil
}

func (f *fakeBlobSource) Blob(_ context.Context, _, sha string) ([]byte, error) {
	f.blobCalls.Add(1)
	f.mu.Lock()
	f.asked = append(f.asked, sha)
	f.mu.Unlock()
	if f.gate != nil {
		<-f.gate
	}
	b, ok := f.blobs[sha]
	if !ok {
		return nil, fmt.Errorf("no blob %s", sha)
	}
	return b, nil
}

func (f *fakeBlobSource) calls() int64 { return f.treeCalls.Load() + f.blobCalls.Load() }

// fakeCommit is a 40-hex id standing for a commit the fake serves.
func fakeCommit(n int) string { return fmt.Sprintf("%040x", 0xc0ffee00+n) }

// addFakeTree registers commit with files (path → content) on src,
// each executable when its path ends .sh.
func addFakeTree(src *fakeBlobSource, commit string, files map[string]string) {
	if src.trees == nil {
		src.trees, src.blobs = map[string]CacheTree{}, map[string][]byte{}
	}
	t := CacheTree{SHA: gitBlobID([]byte("tree "+commit), 40)}
	for path, body := range files {
		sha := gitBlobID([]byte(body), 40)
		src.blobs[sha] = []byte(body)
		mode := "100644"
		if strings.HasSuffix(path, ".sh") {
			mode = "100755"
		}
		t.Entries = append(t.Entries, CacheTreeEntry{Path: path, Mode: mode, SHA: sha})
	}
	src.trees[commit] = t
}

func TestSharedCacheSecondMaterializeIsAllHits(t *testing.T) {
	src := &fakeBlobSource{}
	commit := fakeCommit(1)
	addFakeTree(src, commit, map[string]string{"a.txt": "alpha\n", "dir/b.go": "package b\n", "run.sh": "#!/bin/sh\n"})
	c := &SharedCache{Root: t.TempDir(), Source: src}
	ctx := context.Background()

	if err := c.Materialize(ctx, "acme/widgets", commit, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := c.Counts(); got != (CacheCounts{BlobMisses: 3, TreeMisses: 1}) {
		t.Fatalf("first counts %+v", got)
	}
	before := src.calls()
	dst := t.TempDir()
	if err := c.Materialize(ctx, "acme/widgets", commit, dst); err != nil {
		t.Fatal(err)
	}
	if n := src.calls() - before; n != 0 {
		t.Fatalf("second Materialize called the source %d times, want 0", n)
	}
	if got := c.Counts(); got != (CacheCounts{BlobHits: 3, BlobMisses: 3, TreeHits: 1, TreeMisses: 1}) {
		t.Fatalf("second counts %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "dir", "b.go")); string(b) != "package b\n" {
		t.Fatalf("dir/b.go = %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "run.sh")); fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("run.sh mode %v, want executable", fi.Mode())
	}
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSharedCacheSharedCloneBlobsNeverAskTheSource(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repos := t.TempDir()
	clone := SharedClonePath(repos, "acme/widgets")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, clone, "init", "-q")
	os.WriteFile(filepath.Join(clone, "held.txt"), []byte("in the clone\n"), 0o644)
	gitT(t, clone, "add", ".")
	gitT(t, clone, "commit", "-q", "-m", "one")
	cloneCommit := gitT(t, clone, "rev-parse", "HEAD")
	held := gitT(t, clone, "rev-parse", "HEAD:held.txt")

	// a commit the clone does not have, its tree naming the clone's blob
	// and one the clone lacks
	src := &fakeBlobSource{}
	commit := fakeCommit(2)
	addFakeTree(src, commit, map[string]string{"held.txt": "in the clone\n", "new.txt": "only upstream\n"})
	c := &SharedCache{Root: t.TempDir(), ReposRoot: repos, Source: src}
	ctx := context.Background()

	dst := t.TempDir()
	if err := c.Materialize(ctx, "acme/widgets", commit, dst); err != nil {
		t.Fatal(err)
	}
	for _, sha := range src.asked {
		if sha == held {
			t.Fatalf("source asked for %s, which the shared clone holds", held)
		}
	}
	if len(src.asked) != 1 {
		t.Fatalf("source asked for %v, want only new.txt's blob", src.asked)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "held.txt")); string(b) != "in the clone\n" {
		t.Fatalf("held.txt = %q", b)
	}

	// a commit the clone has: nothing asked of the source at all
	before := src.calls()
	if err := c.Materialize(ctx, "acme/widgets", cloneCommit, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if n := src.calls() - before; n != 0 {
		t.Fatalf("clone's own commit called the source %d times", n)
	}
}

func TestSharedCacheConcurrentMissFillsOnce(t *testing.T) {
	src := &fakeBlobSource{gate: make(chan struct{})}
	commit := fakeCommit(3)
	addFakeTree(src, commit, map[string]string{"only.txt": "one blob\n"})
	c := &SharedCache{Root: t.TempDir(), Source: src}
	ctx := context.Background()

	const n = 2
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.Materialize(ctx, "acme/widgets", commit, t.TempDir())
		}()
	}
	// let both reach the fill before the source answers
	deadline := time.Now().Add(5 * time.Second)
	for src.blobCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(src.gate)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := src.blobCalls.Load(); got != 1 {
		t.Fatalf("source Blob called %d times, want 1", got)
	}
	got := c.Counts()
	if got.BlobMisses != 1 || got.BlobHits != 1 {
		t.Fatalf("counts %+v, want one blob miss and one hit", got)
	}
}

func TestSharedCacheEvictsLeastRecentlyUsedToFit(t *testing.T) {
	src := &fakeBlobSource{}
	old, recent := fakeCommit(4), fakeCommit(5)
	big := strings.Repeat("x", 4096)
	addFakeTree(src, old, map[string]string{"old.txt": big + "old", "shared.txt": "shared\n"})
	addFakeTree(src, recent, map[string]string{"new.txt": big + "new", "shared.txt": "shared\n"})
	clock := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	root := t.TempDir()
	c := &SharedCache{Root: root, Source: src, Now: func() time.Time { return clock }}
	ctx := context.Background()

	if err := c.Materialize(ctx, "acme/widgets", old, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Hour)
	if err := c.Materialize(ctx, "acme/widgets", recent, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	full, _, err := c.scan()
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	c.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	c.CleanGo = func(context.Context, string) error { t.Fatal("go clean run with trees left to evict"); return nil }
	oldTree, newTree := src.trees[old], src.trees[recent]
	var oldBlob string
	for _, e := range oldTree.Entries {
		if e.Path == "old.txt" {
			oldBlob = e.SHA
		}
	}
	// room for everything but the old commit's own entries
	oldOwn := int64(0)
	for _, p := range []string{c.commitPath(old), c.treePath(oldTree.SHA), c.blobPath(oldBlob)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		oldOwn += fi.Size()
	}
	c.MaxBytes = full - oldOwn
	if err := c.Enforce(ctx); err != nil {
		t.Fatal(err)
	}
	total, _, _ := c.scan()
	if total > c.MaxBytes {
		t.Fatalf("cache %d bytes over cap %d", total, c.MaxBytes)
	}
	if _, err := os.Stat(c.treePath(oldTree.SHA)); err == nil {
		t.Fatal("least recently used tree kept")
	}
	if _, err := os.Stat(c.blobPath(oldBlob)); err == nil {
		t.Fatal("least recently used blob kept")
	}
	for _, e := range newTree.Entries {
		if _, err := os.Stat(c.blobPath(e.SHA)); err != nil {
			t.Fatalf("recent blob %s evicted", e.Path)
		}
	}
	if _, err := os.Stat(c.treePath(newTree.SHA)); err != nil {
		t.Fatal("recent tree evicted")
	}
	if len(logs) != 0 {
		t.Fatalf("logged %v under the cap", logs)
	}
}

func TestSharedCacheCleansGoThenSaysOverCapOnce(t *testing.T) {
	root := t.TempDir()
	goDir := filepath.Join(root, "go")
	os.MkdirAll(goDir, 0o755)
	os.WriteFile(filepath.Join(goDir, "big"), make([]byte, 8192), 0o644)
	os.WriteFile(filepath.Join(goDir, "stuck"), make([]byte, 4096), 0o644)
	var logs []string
	cleaned := 0
	c := &SharedCache{
		Root: root, MaxBytes: 1024,
		Logf:    func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		CleanGo: func(_ context.Context, dir string) error { cleaned++; os.Remove(filepath.Join(dir, "big")); return nil },
	}
	for range 2 {
		if err := c.Enforce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if cleaned != 2 {
		t.Fatalf("go clean ran %d times, want 2", cleaned)
	}
	if len(logs) != 1 || logs[0] != "cache over cap: 4.0 KiB of 1.0 KiB" {
		t.Fatalf("logs %q", logs)
	}
}

func TestSharedCacheCountsSurviveRunStateReload(t *testing.T) {
	src := &fakeBlobSource{}
	commit := fakeCommit(6)
	addFakeTree(src, commit, map[string]string{"a": "a\n", "b": "b\n"})
	state := &RunState{Dir: t.TempDir()}
	root := t.TempDir()
	ctx := context.Background()

	c := &SharedCache{Root: root, Source: src, State: state}
	if err := c.Materialize(ctx, "acme/widgets", commit, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := c.Materialize(ctx, "acme/widgets", commit, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	want := CacheCounts{BlobHits: 2, BlobMisses: 2, TreeHits: 1, TreeMisses: 1}
	reloaded := RunState{Dir: state.Dir}
	got, ok := reloaded.CacheCounts()
	if !ok || got != want {
		t.Fatalf("reloaded counts %+v %v, want %+v", got, ok, want)
	}

	// a new process counts on from them
	c2 := &SharedCache{Root: root, Source: src, State: &reloaded}
	if err := c2.Materialize(ctx, "acme/widgets", commit, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	want.BlobHits, want.TreeHits = 4, 2
	if got, _ := reloaded.CacheCounts(); got != want {
		t.Fatalf("counts after restart %+v, want %+v", got, want)
	}
}

func TestSharedCacheDstWritableCacheReadOnly(t *testing.T) {
	src := &fakeBlobSource{}
	commit := fakeCommit(7)
	addFakeTree(src, commit, map[string]string{"f.txt": "f\n", "x.sh": "#!/bin/sh\n"})
	c := &SharedCache{Root: t.TempDir(), Source: src}
	dst := t.TempDir()
	if err := c.Materialize(context.Background(), "acme/widgets", commit, dst); err != nil {
		t.Fatal(err)
	}
	for _, e := range src.trees[commit].Entries {
		fi, err := os.Stat(filepath.Join(dst, e.Path))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o200 == 0 {
			t.Fatalf("dst %s mode %v, want writable", e.Path, fi.Mode())
		}
		if err := os.WriteFile(filepath.Join(dst, e.Path), []byte("changed"), 0o644); err != nil {
			t.Fatalf("write dst %s: %v", e.Path, err)
		}
		ci, err := os.Stat(c.blobPath(e.SHA))
		if err != nil {
			t.Fatal(err)
		}
		if ci.Mode().Perm()&0o222 != 0 {
			t.Fatalf("cache blob %s mode %v, want read-only", e.Path, ci.Mode())
		}
		if b, _ := os.ReadFile(c.blobPath(e.SHA)); string(b) != string(src.blobs[e.SHA]) {
			t.Fatalf("cache blob %s changed by a write to dst", e.Path)
		}
	}
	ti, _ := os.Stat(c.treePath(src.trees[commit].SHA))
	if ti.Mode().Perm()&0o222 != 0 {
		t.Fatalf("cache tree mode %v, want read-only", ti.Mode())
	}
}

func TestSharedCacheRefusesPathOutsideDst(t *testing.T) {
	src := &fakeBlobSource{}
	commit := fakeCommit(8)
	addFakeTree(src, commit, map[string]string{"ok": "ok\n"})
	tr := src.trees[commit]
	tr.Entries[0].Path = "../escape"
	src.trees[commit] = tr
	c := &SharedCache{Root: t.TempDir(), Source: src}
	if err := c.Materialize(context.Background(), "acme/widgets", commit, t.TempDir()); err == nil || !strings.Contains(err.Error(), "leaves the tree") {
		t.Fatalf("err %v, want leaves the tree", err)
	}
}
