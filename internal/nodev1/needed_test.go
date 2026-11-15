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

// movedSHA is a commit the fixture never had: where a ref was when the
// job was offered, before it moved to the fixture's head.
const movedSHA = "1111111111111111111111111111111111111111"

// setRef points the fake code host's ref at sha.
func (g *fakeGitHub) setRef(t *testing.T, ref, sha string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", g.bare, "update-ref", ref, sha).CombinedOutput(); err != nil {
		t.Fatalf("git update-ref %s: %v\n%s", ref, err, out)
	}
}

// endedNoLongerNeeded is the reason the job's lease ended
// no_longer_needed with, after checking it ended no other way: no
// finish, no failure, no hand-back, and its session never started.
func endedNoLongerNeeded(t *testing.T, g *rig, sess *stubSession, job string) string {
	t.Helper()
	lease := g.fake.LeaseOf(job)
	if got := g.fake.Ended(lease); got != "no-longer-needed" {
		t.Fatalf("lease %s ended %q, want no-longer-needed; hand-backs %+v fails %+v", lease, got, g.fake.Calls("hand-back"), g.fake.Calls("fail"))
	}
	// not a failure and not a hand-back: Mesh hears neither
	if n := len(g.fake.Calls("fail")) + len(g.fake.Calls("hand-back")) + len(g.fake.Calls("finish")); n != 0 {
		t.Fatalf("%d finishes, failures and hand-backs beside the no-longer-needed end", n)
	}
	calls := g.fake.Calls("no-longer-needed")
	if len(calls) != 1 || calls[0].LeaseID != lease || calls[0].Auth != "Bearer machine" {
		t.Fatalf("no-longer-needed calls %+v", calls)
	}
	sess.mu.Lock()
	ran := len(sess.specs)
	sess.mu.Unlock()
	if ran != 0 {
		t.Fatalf("the session ran %d time(s) for a job that was no longer needed", ran)
	}
	// the lease's work never started: no checkout was made, and nothing
	// of the lease is left
	if _, err := os.Stat(filepath.Join(g.work, "leases", lease)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lease's directory is still there: %v", err)
	}
	if left := g.rt.relay.Leases(); len(left) != 0 {
		t.Fatalf("grants left open: %v", left)
	}
	var req meshapi.NoLongerNeededRequest
	json.Unmarshal(calls[0].Body, &req)
	return req.Reason
}

// A job whose pull request's head moved after it was offered ends
// no_longer_needed before its work starts, and is not counted as a
// failure (ADR-052; goal a-job-is-still-needed-or-ends-quietly): the
// node reads the head through the lease's own git grant, on the token
// the relay mints, and Mesh hears one no-longer-needed end — no
// hand-back, no fail, no finish — with the reason.
func TestAJobWhosePullRequestHeadMovedEndsNoLongerNeeded(t *testing.T) {
	for _, mode := range []string{"lease", "actor"} {
		t.Run(mode, func(t *testing.T) {
			sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "sess-1"}}
			var g *rig
			if mode == "actor" {
				g, _ = actorRig(t, sess)
			} else {
				g = newRig(t, sess)
			}
			// the pull request's head is the fixture's commit now; the job
			// was offered to validate the commit it was before
			g.github.setRef(t, "refs/pull/12/head", g.github.head)
			job := g.offer(t, func(b *meshapi.Brief) {
				b.Preconditions = []meshapi.Precondition{{Type: meshapi.PreconditionPRHead, Repo: "acme/fixture", Number: 12, SHA: movedSHA}}
			})
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			reason := endedNoLongerNeeded(t, g, sess, job)
			for _, want := range []string{"pull request acme/fixture#12", "is now " + g.github.head, "not " + movedSHA} {
				if !strings.Contains(reason, want) {
					t.Fatalf("the reason %q does not say %q", reason, want)
				}
			}
			// the read went through the relay on the minted git token,
			// which is revoked with the grant
			minted, revoked, failures := g.latchkey.snapshot()
			if len(failures) > 0 || len(minted) == 0 || strings.Join(minted, ",") != strings.Join(revoked, ",") {
				t.Fatalf("minted %v, revoked %v, Latchkey refused %v", minted, revoked, failures)
			}
			for _, a := range g.github.auths() {
				if a != basicAuth(minted[0]) {
					t.Fatalf("the code host saw %q, not the lease's minted token", a)
				}
			}
		})
	}
}

// A branch that moved, or is gone, ends the job the same way.
func TestAJobWhoseRefMovedOrIsGoneEndsNoLongerNeeded(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    meshapi.Precondition
		want string
	}{
		{"moved", meshapi.Precondition{Type: meshapi.PreconditionRefHead, Repo: "acme/fixture", Ref: "main", SHA: movedSHA}, "acme/fixture refs/heads/main is now "},
		{"gone", meshapi.Precondition{Type: meshapi.PreconditionRefHead, Repo: "acme/fixture", Ref: "refs/heads/batch/7", SHA: movedSHA}, "acme/fixture refs/heads/batch/7 is gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := &stubSession{result: SessionResult{SessionID: "sess-1"}}
			g := newRig(t, sess)
			job := g.offer(t, func(b *meshapi.Brief) { b.Preconditions = []meshapi.Precondition{tc.p} })
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			if reason := endedNoLongerNeeded(t, g, sess, job); !strings.Contains(reason, tc.want) {
				t.Fatalf("the reason %q does not say %q", reason, tc.want)
			}
		})
	}
}

// A job whose preconditions still hold runs as it always did, and so
// does one whose precondition the node cannot tell: a type it does not
// know, or a repository the lease's git does not reach. Only a ref the
// code host answered for ends a job.
func TestAJobWhosePreconditionsHoldOrCannotBeToldRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		pre  func(g *rig) []meshapi.Precondition
	}{
		{"both hold", func(g *rig) []meshapi.Precondition {
			return []meshapi.Precondition{
				{Type: meshapi.PreconditionPRHead, Repo: "acme/fixture", Number: 12, SHA: g.github.head},
				{Type: meshapi.PreconditionRefHead, Repo: "Acme/Fixture", Ref: "main", SHA: strings.ToUpper(g.github.head)},
			}
		}},
		{"a type the node does not know", func(g *rig) []meshapi.Precondition {
			return []meshapi.Precondition{{Type: "file_exists", Repo: "acme/fixture", SHA: movedSHA}}
		}},
		{"a repository the grant does not reach", func(g *rig) []meshapi.Precondition {
			return []meshapi.Precondition{{Type: meshapi.PreconditionRefHead, Repo: "acme/elsewhere", Ref: "main", SHA: movedSHA}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "sess-1"}}
			g := newRig(t, sess)
			g.github.setRef(t, "refs/pull/12/head", g.github.head)
			job := g.offer(t, func(b *meshapi.Brief) { b.Preconditions = tc.pre(g) })
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			lease := g.fake.LeaseOf(job)
			if got := g.fake.Ended(lease); got != "finish" {
				t.Fatalf("lease %s ended %q, want finish; no-longer-needed %+v hand-backs %+v", lease, got, g.fake.Calls("no-longer-needed"), g.fake.Calls("hand-back"))
			}
			if n := len(g.fake.Calls("no-longer-needed")); n != 0 {
				t.Fatalf("%d no-longer-needed ends for a job that ran", n)
			}
		})
	}
}

// TestPreconditionRef: the ref each precondition's head is read at.
func TestPreconditionRef(t *testing.T) {
	for _, tc := range []struct {
		vendor string
		p      meshapi.Precondition
		ref    string
	}{
		{"github", meshapi.Precondition{Type: "pr_head", Repo: "a/b", Number: 12}, "refs/pull/12/head"},
		{"", meshapi.Precondition{Type: "pr_head", Repo: "a/b", Number: 12}, "refs/pull/12/head"},
		{"gitlab", meshapi.Precondition{Type: "pr_head", Repo: "a/b", Number: 12}, "refs/merge-requests/12/head"},
		{"bitbucket", meshapi.Precondition{Type: "pr_head", Repo: "a/b", Number: 12}, ""},
		{"github", meshapi.Precondition{Type: "pr_head", Repo: "a/b"}, ""},
		{"github", meshapi.Precondition{Type: "ref_head", Repo: "a/b", Ref: "main"}, "refs/heads/main"},
		{"github", meshapi.Precondition{Type: "ref_head", Repo: "a/b", Ref: "refs/runsheet/chains/9"}, "refs/runsheet/chains/9"},
		{"github", meshapi.Precondition{Type: "ref_head", Repo: "a/b"}, ""},
		{"github", meshapi.Precondition{Type: "nope", Repo: "a/b"}, ""},
	} {
		if ref, what := preconditionRef(tc.vendor, tc.p); ref != tc.ref || what == "" {
			t.Errorf("%s %+v: ref %q (%s), want %q", tc.vendor, tc.p, ref, what, tc.ref)
		}
	}
}
