package nodev1

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

func TestWatchBuildCacheEvictsLiveOverTheCap(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "00")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", sub, err)
	}
	const n = 5
	const fileSize = 50 // two files per entry, 100 bytes/entry
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
