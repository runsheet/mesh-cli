package resources

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestBuildCacheEvictorRemovesLeastRecentlyUsedFirst(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "00")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", sub, err)
	}

	const n = 5
	const fileSize = 50 // two files per entry, 100 bytes/entry
	base := time.Now().Add(-time.Duration(n) * time.Hour)
	keys := make([]string, n)
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
		keys[i] = filepath.Join(sub, hash)
	}

	// Entry 1 (the second-oldest) is "read" — its atime bumped — the
	// instant the evictor is about to consider it, simulating a read
	// landing between the scan and the removal. It must survive.
	touched := keys[1]
	var touchedOnce bool
	ev := &Evictor{Root: root, MaxBytes: 300}
	ev.beforeEvict = func(key string) {
		if key != touched || touchedOnce {
			return
		}
		touchedOnce = true
		now := time.Now()
		for _, suffix := range []string{"-a", "-d"} {
			if err := os.Chtimes(touched+suffix, now, now); err != nil {
				t.Fatalf("chtimes (simulated read) %s: %v", touched+suffix, err)
			}
		}
	}

	res, err := ev.Evict()
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if res.Before != n*2*fileSize {
		t.Fatalf("Before = %d, want %d", res.Before, n*2*fileSize)
	}
	// Cap 300, target fraction 0.9 -> target 270. Oldest-first: 0 is
	// removed (400 left), 1 is skipped as touched (still 400), 2 is
	// removed (300, still over target), 3 is removed (200, at or under
	// target) and the loop stops before ever considering 4.
	if res.After != 200 {
		t.Fatalf("After = %d, want 200", res.After)
	}
	if res.Entries != 3 {
		t.Fatalf("Entries = %d, want 3", res.Entries)
	}
	if res.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1 (the entry touched mid-run)", res.Skipped)
	}
	if !touchedOnce {
		t.Fatalf("beforeEvict hook never fired for the touched entry")
	}

	for _, i := range []int{0, 2, 3} {
		for _, suffix := range []string{"-a", "-d"} {
			p := filepath.Join(sub, fmt.Sprintf("hash%d%s", i, suffix))
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatalf("%s should have been evicted as least recently used, stat err = %v", p, err)
			}
		}
	}
	for _, i := range []int{1, 4} {
		for _, suffix := range []string{"-a", "-d"} {
			p := filepath.Join(sub, fmt.Sprintf("hash%d%s", i, suffix))
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("%s should have been kept: %v", p, err)
			}
		}
	}

	info, err := os.Stat(sub)
	if err != nil || !info.IsDir() {
		t.Fatalf("directory %s was removed; Evict must only ever remove files", sub)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("Root itself was removed: %v", err)
	}
}

// goBinary is the go command the test runs the fixture build with: the
// one on PATH, else GOROOT's own, since the test binary was itself built
// by a go toolchain that may not have put its bin dir on this process's
// PATH.
func goBinary(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	p := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	t.Skip("no go toolchain found on PATH or in GOROOT")
	return ""
}

// withGOCACHE is os.Environ() with any inherited GOCACHE replaced by dir,
// rather than just appended after it — a duplicate env entry is not
// guaranteed to be resolved by taking the last one.
func withGOCACHE(dir string) []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if len(kv) >= len("GOCACHE=") && kv[:len("GOCACHE=")] == "GOCACHE=" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GOCACHE="+dir)
}

// writeFixturePackage writes a tiny buildable Go package (one that
// touches a stdlib import, so a build actually exercises the cache)
// under dir.
func writeFixturePackage(t *testing.T, dir string) {
	t.Helper()
	goMod := "module buildcachefixture\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	main := "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"buildcachefixture\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(main), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
}

// loadSyntheticEntries pads cacheDir with old, synthetic cache-shaped
// entries (an -a/-d pair under a two-hex-character subdirectory, as the
// Go build cache itself lays them out) until at least totalBytes have
// been written, each with an access time well in the past so the real
// entries a build just made are not the first candidates evicted.
func loadSyntheticEntries(t *testing.T, cacheDir string, totalBytes int64) {
	t.Helper()
	const perFile = 1 << 20 // 1MiB
	dir := filepath.Join(cacheDir, "ab")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir synthetic entries dir: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	var written int64
	for i := 0; written < totalBytes; i++ {
		for _, suffix := range []string{"-a", "-d"} {
			name := fmt.Sprintf("synthetic%d%s", i, suffix)
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, make([]byte, perFile), 0o600); err != nil {
				t.Fatalf("seed synthetic entry %s: %v", name, err)
			}
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatalf("chtimes %s: %v", name, err)
			}
			written += perFile
		}
	}
}

func TestGoBuildSucceedsWhileBuildCacheEvictionRuns(t *testing.T) {
	goBin := goBinary(t)

	cacheDir := t.TempDir()
	modDir := t.TempDir()
	writeFixturePackage(t, modDir)

	build := func(extra ...string) ([]byte, error) {
		outDir := t.TempDir()
		args := append([]string{"build", "-o", filepath.Join(outDir, "out")}, extra...)
		args = append(args, ".")
		cmd := exec.Command(goBin, args...)
		cmd.Dir = modDir
		cmd.Env = withGOCACHE(cacheDir)
		return cmd.CombinedOutput()
	}

	// Populate a real GOCACHE by building the fixture package.
	if out, err := build(); err != nil {
		t.Fatalf("seed build: %v\n%s", err, out)
	}

	before, err := DirSize(cacheDir)
	if err != nil {
		t.Fatalf("DirSize: %v", err)
	}

	// Load it over a cap with synthetic old entries, so eviction
	// actually has real work to do while the second build runs.
	loadSyntheticEntries(t, cacheDir, before+16<<20)

	loaded, err := DirSize(cacheDir)
	if err != nil {
		t.Fatalf("DirSize: %v", err)
	}
	maxBytes := loaded / 2
	if maxBytes <= 0 {
		t.Fatalf("loaded cache size %d too small to set up a cap", loaded)
	}

	var wg sync.WaitGroup
	evictErrCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := EvictLRU(cacheDir, maxBytes)
		evictErrCh <- err
	}()

	// -a forces the second build to actually read and write cache
	// entries rather than short-circuit on "already built", so it
	// overlaps meaningfully with the concurrent eviction.
	out, buildErr := build("-a")
	wg.Wait()
	if evictErr := <-evictErrCh; evictErr != nil {
		t.Fatalf("EvictLRU: %v", evictErr)
	}
	if buildErr != nil {
		t.Fatalf("second go build failed while cache eviction ran concurrently: %v\n%s", buildErr, out)
	}
}
