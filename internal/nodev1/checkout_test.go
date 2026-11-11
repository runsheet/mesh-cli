package nodev1

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A base reachable only through a changeset's pin ref -- never from any
// branch -- is still one checkout() can start from: the mirror's own
// fetch takes refs/runsheet/changesets/* (resources.Mirrors.Sync), and
// checkout fetches the base_sha directly before checking it out, since
// neither the mirror's --reference clone nor the plain one transfers an
// object no branch or tag names (goal
// a-leases-base-checkout-resolves-a-base-reachable-only-through-the-mirrors-pin-refs).
func TestACheckoutResolvesABaseReachableOnlyThroughTheMirrorsPinRef(t *testing.T) {
	var head string
	sess := &stubSession{calls: []string{"conform/submit"}, inspect: func(spec SessionSpec) {
		raw, _ := os.ReadFile(filepath.Join(spec.Workspace, ".git", "HEAD"))
		head = strings.TrimSpace(string(raw))
	}}
	g := newRig(t, sess)

	const pinRef = "refs/runsheet/changesets/7603ab67-143c-5b69-af95-b48704538233"
	pinned := bareCommit(t, g.github.bare, g.github.head, "pinned, reachable only through its changeset pin")
	bareSet(t, g.github.bare, pinRef, pinned)
	// main never moves: pinned is on no branch or tag of its own, only
	// on the pin ref -- the same shape resources.Mirrors.Sync's own test
	// builds (TestSyncFetchesTheChangesetPinNamespace).
	if got := g.github.ref("refs/heads/main"); got != g.github.head {
		t.Fatalf("main moved to %s, want %s", got, g.github.head)
	}

	g.offer(t, func(b *meshapi.Brief) {
		b.Code.BaseSHA = pinned
		b.Code.BaseRef = ""
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := run(t, g, ctx); err != nil {
		t.Fatal(err)
	}
	if head != pinned {
		t.Fatalf("checkout HEAD %q, want detached at the pinned base %s", head, pinned)
	}

	mirrors, _ := filepath.Glob(filepath.Join(g.work, "mirrors", "*", "github", "*", "acme", "fixture.git"))
	if len(mirrors) != 1 {
		t.Fatalf("mirrors %v", mirrors)
	}
	if out, err := exec.Command("git", "-C", mirrors[0], "rev-parse", "-q", "--verify", pinRef).Output(); err != nil || strings.TrimSpace(string(out)) != pinned {
		t.Fatalf("the mirror's %s is %q, %v; want %s", pinRef, out, err, pinned)
	}
}
