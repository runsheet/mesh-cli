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

// masterOnly renames bare's one branch, main, to master -- the fixture
// then has no branch literally named "main" at all, master's commit
// unchanged.
func masterOnly(t *testing.T, bare string) {
	t.Helper()
	sha := gitT(t, bare, "rev-parse", "refs/heads/main")
	gitT(t, bare, "update-ref", "refs/heads/master", sha)
	gitT(t, bare, "update-ref", "-d", "refs/heads/main")
	gitT(t, bare, "symbolic-ref", "HEAD", "refs/heads/master")
}

// Mesh names a job's base_ref "main" as its own guess when it does not
// actually know the item's real base (the same guess resolve.go's and
// baseOf's BaseRef once made, goal
// a-job-checks-out-the-repos-default-branch-never-a-hard-coded-main). A
// lease whose origin has no branch named "main" at all -- only master --
// still checks out clean whether its brief names no base_ref, or names
// base_ref "main" with no base_sha: on today's bug, the second case
// instead fetches that literal name, finds it gone, and the job never
// finishes. Both land on master's head, with the same number of requests
// to origin as the clone alone makes -- no extra request for a fetch of
// a "main" that was never there.
func TestDefaultBranchNeverFetchesAMainThatIsntThere(t *testing.T) {
	requests := map[string]int{}
	for _, c := range []struct {
		name   string
		mutate func(*meshapi.Brief)
	}{
		{"no base_ref", func(b *meshapi.Brief) { b.Code.BaseRef = ""; b.Code.BaseSHA = "" }},
		{"base_ref main, no base_sha", func(b *meshapi.Brief) { b.Code.BaseRef = "main"; b.Code.BaseSHA = "" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var head string
			sess := &stubSession{calls: []string{"conform/submit"}, inspect: func(spec SessionSpec) {
				out, err := exec.Command("git", "-C", spec.Workspace, "rev-parse", "HEAD").Output()
				if err != nil {
					t.Fatalf("rev-parse HEAD: %v", err)
				}
				head = strings.TrimSpace(string(out))
			}}
			g := newRig(t, sess)
			masterOnly(t, g.github.bare)
			job := g.offer(t, c.mutate)
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			lease := g.fake.LeaseOf(job)
			if got := g.fake.Ended(lease); got != "finish" {
				t.Fatalf("lease ended %q, want finish", got)
			}
			if head != g.github.head {
				t.Fatalf("checkout HEAD %q, want master's head %s", head, g.github.head)
			}
			requests[c.name] = len(g.github.auths())
		})
	}
	if requests["no base_ref"] != requests["base_ref main, no base_sha"] {
		t.Fatalf("base_ref \"main\" cost %d requests to origin, no base_ref only %d: an explicit fetch of a \"main\" that isn't there",
			requests["base_ref main, no base_sha"], requests["no base_ref"])
	}
}
