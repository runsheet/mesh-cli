package nodev1

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTenantCacheDirsCreatedPerTenant(t *testing.T) {
	root := t.TempDir()
	c := &TenantCache{Root: root}

	a, err := c.Dirs("tenant-a")
	if err != nil {
		t.Fatalf("Dirs(tenant-a): %v", err)
	}
	b, err := c.Dirs("tenant-b")
	if err != nil {
		t.Fatalf("Dirs(tenant-b): %v", err)
	}

	if a.GoMod == b.GoMod || a.Npm == b.Npm || a.Pnpm == b.Pnpm {
		t.Fatalf("tenants share a cache dir: %+v vs %+v", a, b)
	}

	for _, dir := range []string{a.GoMod, a.Npm, a.Pnpm, b.GoMod, b.Npm, b.Pnpm} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Fatalf("%s mode = %o, want 0700", dir, mode)
		}
	}
}

func TestCacheTrimRemovesLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()

	names := []string{"a", "b", "c", "d"}
	base := time.Now().Add(-time.Hour)
	for i, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, 10), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		mtime := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}

	// 40 bytes total; a cap of 20 must evict the two oldest-accessed
	// (a, b) and leave the two most recently accessed (c, d).
	res, err := Trim(dir, 20)
	if err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if res.Before != 40 {
		t.Fatalf("Before = %d, want 40", res.Before)
	}
	if res.After > 20 {
		t.Fatalf("After = %d, want <= 20", res.After)
	}
	if len(res.Removed) == 0 {
		t.Fatalf("Removed is empty, want the oldest entries named")
	}

	for _, removed := range []string{"a", "b"} {
		if _, err := os.Stat(filepath.Join(dir, removed)); !os.IsNotExist(err) {
			t.Fatalf("%s should have been removed as least recently used, stat err = %v", removed, err)
		}
	}
	for _, kept := range []string{"c", "d"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Fatalf("%s should have been kept: %v", kept, err)
		}
	}
}

func TestCacheTrimNoopUnderBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, make([]byte, 10), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	res, err := Trim(dir, 1000)
	if err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v, want none removed when already under budget", res.Removed)
	}
	if res.Before != 10 || res.After != 10 {
		t.Fatalf("Before/After = %d/%d, want 10/10", res.Before, res.After)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file removed though the dir was under budget: %v", err)
	}
}
