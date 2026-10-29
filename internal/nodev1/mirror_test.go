package nodev1

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
