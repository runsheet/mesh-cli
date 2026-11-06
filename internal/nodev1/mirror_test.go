package nodev1

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// breakMirror corrupts the mirror at dir the one way that passes a fetch
// (so Sync leaves it in place) but fails a --reference clone outright: a
// shallow file naming a commit the mirror holds, sha. git fetch tolerates
// it; git clone --reference refuses a shallow alternate before any
// network use.
func breakMirror(t *testing.T, dir, sha string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "shallow"), []byte(sha+"\n"), 0o600); err != nil {
		t.Fatalf("writing the mirror's shallow file: %v", err)
	}
}

// capturedLogf wraps t.Logf and keeps every line for a test to search.
func capturedLogf(t *testing.T) (func(string, ...any), func() []string) {
	var mu sync.Mutex
	var lines []string
	return func(format string, args ...any) {
			line := fmt.Sprintf(format, args...)
			t.Logf("%s", line)
			mu.Lock()
			lines = append(lines, line)
			mu.Unlock()
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), lines...)
		}
}

// A lease's checkout goes through its tenant's mirror of the repository
// (Mesh v1 2.8): the mirror is filled through the lease's relay, names
// no remote, and outlives the lease; the checkout copies what it
// borrowed, so it never depends on the mirror.
func TestACheckoutGoesThroughTheTenantsMirror(t *testing.T) {
	var alternates error
	var head string
	sess := &stubSession{calls: []string{"conform/submit"}, inspect: func(spec SessionSpec) {
		_, alternates = os.Stat(filepath.Join(spec.Workspace, ".git", "objects", "info", "alternates"))
		raw, _ := os.ReadFile(filepath.Join(spec.Workspace, ".git", "HEAD"))
		head = strings.TrimSpace(string(raw))
	}}
	g := newRig(t, sess)
	g.offer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := run(t, g, ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(alternates, os.ErrNotExist) {
		t.Fatalf("the checkout still borrows from the mirror: %v", alternates)
	}
	if head != g.github.head {
		t.Fatalf("checkout HEAD %q, want %s", head, g.github.head)
	}
	mirrors, _ := filepath.Glob(filepath.Join(g.work, "mirrors", "*", "github", "*", "acme", "fixture.git"))
	if len(mirrors) != 1 {
		t.Fatalf("mirrors %v", mirrors)
	}
	cfg, _ := os.ReadFile(filepath.Join(mirrors[0], "config"))
	if strings.Contains(string(cfg), "remote") || strings.Contains(string(cfg), "/j/") {
		t.Fatalf("the mirror names its lease's relay:\n%s", cfg)
	}
	minted, _, _ := g.latchkey.snapshot()
	for _, a := range g.github.auths() {
		if a != basicAuth(minted[0]) {
			t.Fatalf("the mirror's fetch reached the vendor on %q, not the lease's token", a)
		}
	}
}

// A mirror that syncs clean but breaks the --reference clone does not
// stop the checkout: the clone is retried without it, and the node log
// names the mirror's directory and the clone's error (goal
// a-bad-mirror-never-stops-a-checkout).
func TestACheckoutRetriesWithoutABrokenMirror(t *testing.T) {
	var head string
	sess := &stubSession{calls: []string{"conform/submit"}, inspect: func(spec SessionSpec) {
		raw, _ := os.ReadFile(filepath.Join(spec.Workspace, ".git", "HEAD"))
		head = strings.TrimSpace(string(raw))
	}}
	g := newRig(t, sess)

	g.offer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := run(t, g, ctx); err != nil {
		t.Fatalf("the first lease: %v", err)
	}
	mirrors, _ := filepath.Glob(filepath.Join(g.work, "mirrors", "*", "github", "*", "acme", "fixture.git"))
	if len(mirrors) != 1 {
		t.Fatalf("mirrors %v", mirrors)
	}
	mirrorDir := mirrors[0]
	breakMirror(t, mirrorDir, g.github.head)

	logf, logged := capturedLogf(t)
	g.rt.Logf = logf

	g.offer(t, nil)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	if err := run(t, g, ctx2); err != nil {
		t.Fatalf("the second lease: %v", err)
	}
	if head != g.github.head {
		t.Fatalf("checkout HEAD %q, want %s", head, g.github.head)
	}
	var found bool
	for _, line := range logged() {
		if strings.Contains(line, mirrorDir) && strings.Contains(line, "shallow") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no log line names the mirror's directory %s and the clone's error; got %v", mirrorDir, logged())
	}
}

// After a lease's clone fails against a mirror, the next lease for the
// same tenant and repository builds a fresh mirror rather than reusing
// the one the checkout found broken (goal
// a-bad-mirror-never-stops-a-checkout).
func TestTheNextLeaseDoesNotBorrowFromAFailedMirrorUntilRebuilt(t *testing.T) {
	sess := &stubSession{calls: []string{"conform/submit"}}
	g := newRig(t, sess)

	g.offer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := run(t, g, ctx); err != nil {
		t.Fatalf("the first lease: %v", err)
	}
	mirrorGlob := filepath.Join(g.work, "mirrors", "*", "github", "*", "acme", "fixture.git")
	mirrors, _ := filepath.Glob(mirrorGlob)
	if len(mirrors) != 1 {
		t.Fatalf("mirrors %v", mirrors)
	}
	mirrorDir := mirrors[0]
	breakMirror(t, mirrorDir, g.github.head)

	g.offer(t, nil)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	if err := run(t, g, ctx2); err != nil {
		t.Fatalf("the second lease: %v", err)
	}
	aside, _ := filepath.Glob(mirrorDir + ".bad-*")
	if len(aside) != 1 {
		t.Fatalf("the broken mirror was not set aside: %v", aside)
	}

	g.offer(t, nil)
	ctx3, cancel3 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel3()
	if err := run(t, g, ctx3); err != nil {
		t.Fatalf("the third lease: %v", err)
	}
	fresh, _ := filepath.Glob(mirrorGlob)
	if len(fresh) != 1 {
		t.Fatalf("fresh mirror %v", fresh)
	}
	if fresh[0] != mirrorDir {
		t.Fatalf("the fresh mirror is at %s, want %s", fresh[0], mirrorDir)
	}
	if _, err := os.Stat(filepath.Join(fresh[0], "shallow")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fresh mirror still carries the broken one's shallow file: %v", err)
	}
	stillAside, _ := filepath.Glob(mirrorDir + ".bad-*")
	if len(stillAside) != 1 {
		t.Fatalf("the set-aside mirror should remain for inspection: %v", stillAside)
	}
}
