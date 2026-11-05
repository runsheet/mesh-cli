package nodev1

import (
	"io/fs"
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

func TestTenantCacheDirsDistinctForDegenerateIds(t *testing.T) {
	root := t.TempDir()
	c := &TenantCache{Root: root}

	// Both ids are non-empty but sanitize away to nothing under
	// cacheSegment's rules; they must still land in different
	// directories rather than colliding on a shared fallback name.
	dots, err := c.Dirs("..")
	if err != nil {
		t.Fatalf(`Dirs(".."): %v`, err)
	}
	bangs, err := c.Dirs("!!!")
	if err != nil {
		t.Fatalf(`Dirs("!!!"): %v`, err)
	}

	if dots.GoMod == bangs.GoMod || dots.Npm == bangs.Npm || dots.Pnpm == bangs.Pnpm {
		t.Fatalf("degenerate tenant ids share a cache dir: %+v vs %+v", dots, bangs)
	}
}

func TestCacheTrimRemovesLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()

	// Access order, oldest to newest: a, b, c, d. Modification order is
	// the reverse (d, c, b, a) so a Trim that evicted by ModTime instead
	// of access time would remove d and c, the opposite of what this
	// test asserts, and fail.
	names := []string{"a", "b", "c", "d"}
	base := time.Now().Add(-4 * time.Hour)
	for i, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, 10), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		atime := base.Add(time.Duration(i) * time.Hour)
		mtime := base.Add(time.Duration(len(names)-1-i) * time.Hour)
		if err := os.Chtimes(path, atime, mtime); err != nil {
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

// seedOverlayFile writes content under leaseDir/cache/<mount>/upper/rel,
// as a lease's sandboxed session would write into its overlay's upper
// dir (cachemount.go's leaseCacheMounts).
func seedOverlayFile(t *testing.T, leaseDir, mount, rel string, content []byte) {
	t.Helper()
	path := filepath.Join(leaseDir, "cache", mount, "upper", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("seed %s: %v", rel, err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("seed %s: %v", rel, err)
	}
}

func TestPromoteMergesLeaseWritesIntoTenantCache(t *testing.T) {
	root := t.TempDir()
	leaseDir := t.TempDir()
	tc := &TenantCache{Root: root}

	seedOverlayFile(t, leaseDir, "gomod", "cache/download/example.com/@v/v1.0.0.zip", []byte("a module"))
	seedOverlayFile(t, leaseDir, "npm", "example-pkg/-/example-pkg-1.0.0.tgz", []byte("an npm tarball"))
	seedOverlayFile(t, leaseDir, "pnpm", "v3/files/ab/cdef", []byte("a pnpm store entry"))

	if _, err := tc.PromoteAll("acme", leaseDir); err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}

	dirs, err := tc.Dirs("acme")
	if err != nil {
		t.Fatalf("Dirs(acme): %v", err)
	}
	for _, want := range []struct {
		dir, rel string
		content  string
	}{
		{dirs.GoMod, "cache/download/example.com/@v/v1.0.0.zip", "a module"},
		{dirs.Npm, "example-pkg/-/example-pkg-1.0.0.tgz", "an npm tarball"},
		{dirs.Pnpm, "v3/files/ab/cdef", "a pnpm store entry"},
	} {
		got, err := os.ReadFile(filepath.Join(want.dir, want.rel))
		if err != nil {
			t.Fatalf("promoted file %s/%s not found: %v", want.dir, want.rel, err)
		}
		if string(got) != want.content {
			t.Fatalf("promoted file %s/%s = %q, want %q", want.dir, want.rel, got, want.content)
		}
	}
}

func TestPromoteThenTrimRespectsBudget(t *testing.T) {
	root := t.TempDir()
	leaseDir := t.TempDir()
	const maxBytes = 20
	tc := &TenantCache{Root: root, MaxBytes: maxBytes}

	// Four 10-byte files, 40 bytes total: well over the 20-byte budget,
	// so promoting them must leave the tenant's shared npm cache dir
	// trimmed back to at or under it.
	for _, name := range []string{"a", "b", "c", "d"} {
		seedOverlayFile(t, leaseDir, "npm", name, make([]byte, 10))
	}

	results, err := tc.PromoteAll("acme", leaseDir)
	if err != nil {
		t.Fatalf("PromoteAll: %v", err)
	}
	if got := results["npm"].Promoted; got != 4 {
		t.Fatalf("Promoted = %d, want 4", got)
	}

	dirs, err := tc.Dirs("acme")
	if err != nil {
		t.Fatalf("Dirs(acme): %v", err)
	}
	var total int64
	if err := filepath.WalkDir(dirs.Npm, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", dirs.Npm, err)
	}
	if total > maxBytes {
		t.Fatalf("tenant npm cache holds %d bytes after promote, want <= %d", total, maxBytes)
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
