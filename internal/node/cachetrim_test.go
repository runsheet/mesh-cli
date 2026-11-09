package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cacheWithGo is a cache root whose Go build cache holds n files of size
// bytes each, its CleanGo emptying go/ and counting the runs.
func cacheWithGo(t *testing.T, n, size int, max int64, cleaned *int) *SharedCache {
	t.Helper()
	root := t.TempDir()
	goDir := filepath.Join(root, "go")
	if err := os.MkdirAll(goDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(goDir, fmt.Sprintf("o%02d", i)), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &SharedCache{Root: root, MaxBytes: max, CleanGo: func(_ context.Context, dir string) error {
		*cleaned++
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
		return nil
	}}
}

// A cache root over the cap is trimmed below it, logged `cache: trimmed
// X to Y`, and the reading left in node.cachesize with the scratch
// root's size.
func TestCacheTrimTrimsAnOverCapCacheBelowIt(t *testing.T) {
	var cleaned int
	c := cacheWithGo(t, 10, 1000, 5000, &cleaned)
	// a tree entry, evicted first, still not enough without go clean
	if err := os.MkdirAll(filepath.Join(c.Root, "trees", "aa"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Root, "trees", "aa", "t"), make([]byte, 500), 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, "left"), make([]byte, 300), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &RunState{Dir: t.TempDir()}
	var logs []string
	trim := &CacheTrim{Cache: c, ScratchRoot: scratch, State: state, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	now := time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)
	if !trim.RunIfDue(context.Background(), now) {
		t.Fatal("first run did not run")
	}
	size, err := c.Size()
	if err != nil {
		t.Fatal(err)
	}
	if size >= c.MaxBytes {
		t.Fatalf("size %d after the trim, cap %d", size, c.MaxBytes)
	}
	if cleaned != 1 {
		t.Fatalf("go clean ran %d times", cleaned)
	}
	want := "cache: trimmed " + FormatBytes(10500) + " to " + FormatBytes(size)
	if !containsLine(logs, want) {
		t.Fatalf("logs %q, want %q", logs, want)
	}
	r, ok := state.CacheSize()
	if !ok {
		t.Fatal("no reading recorded")
	}
	if r.Root != c.Root || r.Bytes != size || r.Cap != 5000 || r.ScratchRoot != scratch || r.ScratchBytes != 300 || !r.At.Equal(now) {
		t.Fatalf("reading %+v", r)
	}
}

// A cache root under the cap is untouched: nothing evicted, no go clean,
// no trim line — and still measured.
func TestCacheTrimLeavesACacheUnderTheCap(t *testing.T) {
	var cleaned int
	c := cacheWithGo(t, 3, 1000, 5000, &cleaned)
	state := &RunState{Dir: t.TempDir()}
	var logs []string
	trim := &CacheTrim{Cache: c, State: state, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	trim.RunIfDue(context.Background(), time.Now())
	if cleaned != 0 || len(logs) != 0 {
		t.Fatalf("cleaned %d, logs %q", cleaned, logs)
	}
	ents, _ := os.ReadDir(filepath.Join(c.Root, "go"))
	if len(ents) != 3 {
		t.Fatalf("go/ holds %d files, want 3", len(ents))
	}
	if r, ok := state.CacheSize(); !ok || r.Bytes != 3000 || r.ScratchRoot != "" {
		t.Fatalf("reading %+v %v", r, ok)
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
