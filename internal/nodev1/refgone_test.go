package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// TestRefGone: a base ref's fetch that failed is the ref gone only when
// git said it could not find the remote ref and a fresh ls-remote
// through the same origin answers with the repository's HEAD and no
// such ref (ADR-052 amendment 2026-11-16). Every other failure, an
// origin that does not answer and one that advertises nothing, is not.
func TestRefGone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	sh := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	sh(src, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(src, "add", ".")
	sh(src, "commit", "-q", "-m", "one")
	origin := filepath.Join(root, "origin.git")
	sh(root, "clone", "-q", "--bare", src, origin)
	sh(origin, "update-ref", "refs/heads/mesh/there", "refs/heads/main")

	git := leaseGitRun(context.Background())
	clone := func(name, from string) string {
		t.Helper()
		ws := filepath.Join(root, name)
		if err := os.MkdirAll(ws, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := git(ws, "clone", "-q", from, "."); err != nil {
			t.Fatal(err)
		}
		return ws
	}
	ws := clone("ws", origin)

	for _, ref := range []string{"mesh/never-pushed", "refs/heads/mesh/never-pushed"} {
		_, err := git(ws, "fetch", "-q", "origin", ref)
		if err == nil {
			t.Fatalf("fetching %s found it", ref)
		}
		if !refGone(git, ws, ref, err) {
			t.Fatalf("%s: a ref the origin does not have is not read as gone: %v", ref, err)
		}
	}
	if _, err := git(ws, "fetch", "-q", "origin", "mesh/there"); err != nil {
		t.Fatalf("a ref the origin has: %v", err)
	}
	// git said it could not find the ref, but it is there now (pushed
	// since the fetch): the fetch's error stands
	notFound := errors.New("git fetch: exit status 128: fatal: couldn't find remote ref mesh/there")
	if refGone(git, ws, "mesh/there", notFound) {
		t.Fatal("a ref the origin has is read as gone")
	}
	// any other failure of the fetch is not the ref gone
	other := errors.New("git fetch: exit status 128: fatal: unable to access 'https://relay/x.git/': Could not resolve host: relay")
	if refGone(git, ws, "mesh/never-pushed", other) {
		t.Fatal("a fetch that failed for another reason is read as the ref gone")
	}
	if refGone(git, ws, "mesh/never-pushed", nil) {
		t.Fatal("no failure is read as the ref gone")
	}
	// an origin that does not answer the ls-remote: not gone, whatever
	// the fetch said
	dead := clone("dead", origin)
	if _, err := git(dead, "remote", "set-url", "origin", filepath.Join(root, "nowhere.git")); err != nil {
		t.Fatal(err)
	}
	if refGone(git, dead, "mesh/never-pushed", errors.New("git fetch: exit status 128: fatal: couldn't find remote ref mesh/never-pushed")) {
		t.Fatal("an origin that does not answer is read as the ref gone")
	}
	// an origin that advertises no HEAD — an empty answer — is not read
	// as the ref gone either
	empty := filepath.Join(root, "empty.git")
	sh(root, "init", "-q", "--bare", empty)
	ews := clone("ews", empty)
	_, err := git(ews, "fetch", "-q", "origin", "mesh/never-pushed")
	if err == nil {
		t.Fatal("fetching from an empty origin found the ref")
	}
	if refGone(git, ews, "mesh/never-pushed", err) {
		t.Fatal("an origin that advertises nothing is read as the ref gone")
	}
}

// A validation whose ref the code host no longer has — a build's branch
// that was never pushed, a candidate deleted while the job was queued —
// ends no_longer_needed at its checkout, before its session, instead of
// being handed back cannot_run for Mesh to offer again (ADR-052
// amendment 2026-11-16; on 16 Nov dl360 handed one such job back 78
// times). No hand-back, no fail, no finish; the reason names the ref.
func TestAValidationWhoseRefIsGoneEndsNoLongerNeeded(t *testing.T) {
	for _, mode := range []string{"lease", "actor"} {
		t.Run(mode, func(t *testing.T) {
			sess := &stubSession{result: SessionResult{SessionID: "sess-1"}}
			var g *rig
			if mode == "actor" {
				g, _ = actorRig(t, sess)
			} else {
				g = newRig(t, sess)
			}
			job := g.offer(t, func(b *meshapi.Brief) {
				b.Kind = "runsheet/validate"
				b.Code.BaseRef = "mesh/never-pushed-1"
			})
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			reason := endedNoLongerNeeded(t, g, sess, job)
			if !strings.Contains(reason, "acme/fixture mesh/never-pushed-1 is gone") {
				t.Fatalf("the reason %q does not name the gone ref", reason)
			}
		})
	}
}

// Any other kind whose base ref is gone is handed back cannot_run
// exactly as before: a rebase onto a dependency's branch deleted when it
// merged is not a validation, and ending it quietly would only have the
// product ask for it again.
func TestAnotherKindWhoseRefIsGoneIsHandedBackAsBefore(t *testing.T) {
	for _, kind := range []string{"runsheet/rebase", "runsheet/build", "conform/echo"} {
		t.Run(kind, func(t *testing.T) {
			sess := &stubSession{result: SessionResult{SessionID: "sess-1"}}
			g := newRig(t, sess)
			job := g.offer(t, func(b *meshapi.Brief) {
				b.Kind = kind
				b.Code.BaseRef = "mesh/never-pushed-1"
			})
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			lease := g.fake.LeaseOf(job)
			if got := g.fake.Ended(lease); got != "hand-back" {
				t.Fatalf("lease %s ended %q, want hand-back", lease, got)
			}
			if n := len(g.fake.Calls("no-longer-needed")); n != 0 {
				t.Fatalf("%d no-longer-needed ends for a %s", n, kind)
			}
			calls := g.fake.Calls("hand-back")
			if len(calls) != 1 {
				t.Fatalf("hand-backs %+v", calls)
			}
			var hb meshapi.HandBackRequest
			json.Unmarshal(calls[0].Body, &hb)
			if hb.Cause != meshapi.CauseCannotRun || !strings.Contains(hb.Reason, "checking out acme/fixture: git fetch:") ||
				!strings.Contains(hb.Reason, "couldn't find remote ref mesh/never-pushed-1") {
				t.Fatalf("hand-back %+v, want cannot_run with the fetch's own words", hb)
			}
		})
	}
}
