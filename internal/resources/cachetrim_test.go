package resources

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/scheduler"
)

func fillCache(t *testing.T, root string, n int) {
	t.Helper()
	os.MkdirAll(filepath.Join(root, "00"), 0o700)
	if err := os.WriteFile(filepath.Join(root, "00", "a-d"), make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
}

func emptyCache(_ context.Context, root string) error {
	return os.RemoveAll(filepath.Join(root, "00"))
}

// A cache under its cap is left alone.
func TestTrimLeavesACacheUnderItsCap(t *testing.T) {
	root := t.TempDir()
	fillCache(t, root, 100)
	tr := &CacheTrim{Root: root, MaxBytes: 1000, Scheduler: scheduler.New(scheduler.Capacity{}), Clean: func(context.Context, string) error {
		t.Fatal("cleaned a cache under its cap")
		return nil
	}}
	res, err := tr.Once(context.Background())
	if err != nil || res.Trimmed || res.Before != 100 {
		t.Fatalf("%+v %v", res, err)
	}
}

// A cache over its cap is cleaned only once the trim holds it alone: it
// waits for the lease building with it, and no new lease builds while it
// waits or cleans (the 2026-10-28 go clean -cache under live
// validations).
func TestTrimWaitsForTheBuildsAndHoldsTheCacheAlone(t *testing.T) {
	root := t.TempDir()
	fillCache(t, root, 2000)
	s := scheduler.New(scheduler.Capacity{})
	build, _ := s.TryTake(scheduler.Ask{Owner: "lse_1", Cache: scheduler.CacheShared})
	var mu sync.Mutex
	var during []scheduler.Held
	tr := &CacheTrim{Root: root, MaxBytes: 1000, Scheduler: s, TrimWait: 5 * time.Second, Clean: func(ctx context.Context, root string) error {
		mu.Lock()
		during = s.Leases()
		mu.Unlock()
		if _, err := s.TryTake(scheduler.Ask{Owner: "lse_2", Cache: scheduler.CacheShared}); err == nil {
			t.Error("a lease built with the cache while it was cleaned")
		}
		return emptyCache(ctx, root)
	}}
	done := make(chan TrimResult, 1)
	go func() {
		res, err := tr.Once(context.Background())
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case <-done:
		t.Fatal("the trim cleaned under a live build")
	case <-time.After(100 * time.Millisecond):
	}
	build.Release()
	res := <-done
	if !res.Trimmed || res.Before != 2000 || res.After != 0 {
		t.Fatalf("%+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(during) != 1 || during[0].Owner != "cache-trim" || during[0].Ask.Cache != scheduler.CacheExclusive {
		t.Fatalf("the ledger while cleaning: %+v", during)
	}
	if held := s.Leases(); len(held) != 0 {
		t.Fatalf("the trim kept %+v", held)
	}
}

// A trim whose wait runs out skips the round and says why; the builds go
// on.
func TestTrimSkipsWhenItsWaitRunsOut(t *testing.T) {
	root := t.TempDir()
	fillCache(t, root, 2000)
	s := scheduler.New(scheduler.Capacity{})
	if _, err := s.TryTake(scheduler.Ask{Owner: "lse_1", Cache: scheduler.CacheShared}); err != nil {
		t.Fatal(err)
	}
	var logged []string
	tr := &CacheTrim{Root: root, MaxBytes: 1000, Scheduler: s, TrimWait: 50 * time.Millisecond,
		Clean: func(context.Context, string) error { t.Fatal("cleaned under a build"); return nil },
		Logf:  func(f string, a ...any) { logged = append(logged, f) }}
	res, err := tr.Once(context.Background())
	if err != nil || res.Trimmed || !strings.Contains(res.Skipped, "builds held it") {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := s.TryTake(scheduler.Ask{Owner: "lse_2", Cache: scheduler.CacheShared}); err != nil {
		t.Fatalf("a build after the skipped trim: %v", err)
	}
	if len(logged) != 1 {
		t.Fatalf("logged %v", logged)
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 3 << 30: "3.0 GiB"} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
