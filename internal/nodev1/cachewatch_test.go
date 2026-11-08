package nodev1

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/resources"
)

func TestReadBuildCacheConfigDefaultsMaxGBToShareOfFreeSpace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")

	free := func() (uint64, error) { return 100 * (1 << 30), nil }

	// no node.yaml at all: both default, max_gb from the injected
	// free-space reading
	cfg, err := ReadBuildCacheConfig(path, free)
	if err != nil {
		t.Fatalf("ReadBuildCacheConfig: %v", err)
	}
	wantMaxGB := 100 * DefaultCacheFreeShare
	if cfg.MaxGB != wantMaxGB {
		t.Fatalf("MaxGB = %v, want %v", cfg.MaxGB, wantMaxGB)
	}
	if cfg.MinFreeGB != DefaultMinFreeGB {
		t.Fatalf("MinFreeGB = %v, want %v", cfg.MinFreeGB, DefaultMinFreeGB)
	}

	// a node.yaml naming both uses them unchanged, never reading free
	// space at all
	if err := os.WriteFile(path, []byte("cache:\n  max_gb: 12.5\n  min_free_gb: 7\n"), 0o600); err != nil {
		t.Fatalf("write node.yaml: %v", err)
	}
	calledFree := false
	cfg, err = ReadBuildCacheConfig(path, func() (uint64, error) {
		calledFree = true
		return 0, nil
	})
	if err != nil {
		t.Fatalf("ReadBuildCacheConfig: %v", err)
	}
	if cfg.MaxGB != 12.5 || cfg.MinFreeGB != 7 {
		t.Fatalf("cfg = %+v, want {MaxGB:12.5 MinFreeGB:7}", cfg)
	}
	if calledFree {
		t.Fatal("ReadBuildCacheConfig: read free space though node.yaml named max_gb")
	}
}

// seedBuildCache writes n Go build cache entries (an -a/-d pair of
// fileSize bytes each, under root/00) with access times an hour apart,
// oldest first.
func seedBuildCache(t *testing.T, root string, n, fileSize int) {
	t.Helper()
	sub := filepath.Join(root, "00")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", sub, err)
	}
	base := time.Now().Add(-time.Duration(n) * time.Hour)
	for i := 0; i < n; i++ {
		hash := fmt.Sprintf("hash%d", i)
		aPath := filepath.Join(sub, hash+"-a")
		dPath := filepath.Join(sub, hash+"-d")
		for _, p := range []string{aPath, dPath} {
			if err := os.WriteFile(p, make([]byte, fileSize), 0o600); err != nil {
				t.Fatalf("seed %s: %v", p, err)
			}
		}
		atime := base.Add(time.Duration(i) * time.Hour)
		for _, p := range []string{aPath, dPath} {
			if err := os.Chtimes(p, atime, atime); err != nil {
				t.Fatalf("chtimes %s: %v", p, err)
			}
		}
	}
}

func TestWatchBuildCacheEvictsLiveOverTheCap(t *testing.T) {
	root := t.TempDir()
	seedBuildCache(t, root, 5, 50) // two files per entry, 100 bytes/entry
	// total 500 bytes; a cap of 300 targets 0.9*300 = 270

	r := &Runtime{BuildCache: root}
	want := BuildCacheConfig{MaxGB: 300.0 / (1 << 30), MinFreeGB: 42}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.WatchBuildCache(ctx, func() (BuildCacheConfig, error) { return want, nil }, time.Hour, nil)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		size, err := resources.DirSize(root)
		if err != nil {
			t.Fatalf("DirSize: %v", err)
		}
		if size <= 270 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("build cache: still %d bytes after waiting, want <= 270", size)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	r.mu.Lock()
	got := r.buildCacheConfig
	r.mu.Unlock()
	if got != want {
		t.Fatalf("r.buildCacheConfig = %+v, want %+v", got, want)
	}
}

// Run, as `mesh node run` starts it (BuildCacheConfigPath set,
// BuildCache left ""), pins BuildCache to WorkRoot/build-cache -- the
// path every lease mounts as GOCACHE -- starts the watcher, logs its cap
// and floor once, and evicts a cache already over node.yaml's max_gb
// toward it. Before, BuildCache stayed "" and evictBuildCache evicted
// nothing while the leases filled WorkRoot/build-cache unbounded.
func TestRunStartsTheBuildCacheWatcherOnTheMountedCache(t *testing.T) {
	g := newRig(t, &stubSession{})
	logs := &logLines{t: t}
	g.rt.Logf = logs.logf
	cache := filepath.Join(g.work, "build-cache")
	seedBuildCache(t, cache, 5, 50) // 500 bytes
	nodeYAML := filepath.Join(t.TempDir(), "node.yaml")
	maxGB := 300.0 / (1 << 30) // targets 0.9*300 = 270 bytes
	if err := os.WriteFile(nodeYAML, []byte(fmt.Sprintf("cache:\n  max_gb: %v\n  min_free_gb: 0.000001\n", maxGB)), 0o600); err != nil {
		t.Fatalf("write node.yaml: %v", err)
	}
	g.rt.BuildCacheConfigPath = nodeYAML
	g.rt.BuildCachePoll = time.Hour
	g.rt.Disk = &fakeDiskProbe{free: 100 * (1 << 30)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		size, err := resources.DirSize(cache)
		if err != nil {
			t.Fatalf("DirSize: %v", err)
		}
		if size <= 270 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("build cache %s: still %d bytes, want <= 270: Run never evicted it", cache, size)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}

	if g.rt.BuildCache != cache {
		t.Fatalf("BuildCache = %q, want %q (the path the leases mount)", g.rt.BuildCache, cache)
	}
	out := logs.all()
	for _, want := range []string{
		"nodev1: build cache: watching " + cache,
		"disk floor",
		"nodev1: build cache: evicted ",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("logs: want a line with %q, got:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "nodev1: build cache: watching "); n != 1 {
		t.Fatalf("logs: the start line %d times, want once", n)
	}
}
