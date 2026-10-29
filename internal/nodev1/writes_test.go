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

// A lease whose session pushed through the relay and then failed is not
// handed back cannot_run, which would have Mesh run the job again from
// scratch into a push that already landed (TestWritePath, 2026-10-30):
// it fails with cause infrastructure, its reason naming the push.
func TestALeaseThatPushedThenFailedIsNotHandedBackCannotRun(t *testing.T) {
	sess := &stubSession{err: errors.New("claude: mesh mcp-relay: dial unix: connect: no such file or directory")}
	sess.inspect = func(spec SessionSpec) {
		commitIn(t, spec.Workspace, "two")
		if text, refused, err := callTool(context.Background(), spec.Servers[NodeToolsServer], PushTool, map[string]any{"ref": "mesh/j-1"}); err != nil || refused {
			t.Errorf("the push: %s %v", text, err)
		}
	}
	g := newRig(t, sess)
	job := g.offer(t, writeBrief)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.github.ref("refs/heads/mesh/j-1") == "" {
		t.Fatal("the push did not land")
	}
	lease := g.fake.LeaseOf(job)
	if hb := g.fake.Calls("hand-back"); len(hb) != 0 {
		t.Fatalf("the lease was handed back: %s", hb[0].Body)
	}
	calls := g.fake.Calls("fail")
	if g.fake.Ended(lease) != "fail" || len(calls) != 1 {
		t.Fatalf("lease %s ended %q", lease, g.fake.Ended(lease))
	}
	var fr meshapi.FailRequest
	json.Unmarshal(calls[0].Body, &fr)
	if fr.Cause != meshapi.CauseInfrastructure || !strings.Contains(fr.Reason, "no such file or directory") ||
		!strings.Contains(fr.Reason, "pushed refs/heads/mesh/j-1 to acme/fixture") {
		t.Fatalf("the failure is %+v", fr)
	}
}

// A lease that failed before any write is handed back cannot_run, as
// before: a refused push is no write.
func TestALeaseThatFailedBeforeAnyWriteIsHandedBackCannotRun(t *testing.T) {
	sess := &stubSession{err: errors.New("claude: the session ended with an error")}
	sess.inspect = func(spec SessionSpec) {
		commitIn(t, spec.Workspace, "two")
		if _, refused, err := callTool(context.Background(), spec.Servers[NodeToolsServer], PushTool, map[string]any{"ref": "main"}); err != nil || !refused {
			t.Errorf("a push to main: refused %v %v", refused, err)
		}
	}
	g := newRig(t, sess)
	job := g.offer(t, writeBrief)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := g.fake.Calls("hand-back")
	if g.fake.Ended(g.fake.LeaseOf(job)) != "hand-back" || len(calls) != 1 {
		t.Fatalf("lease ended %q", g.fake.Ended(g.fake.LeaseOf(job)))
	}
	var hb meshapi.HandBackRequest
	json.Unmarshal(calls[0].Body, &hb)
	if hb.Cause != meshapi.CauseCannotRun || strings.Contains(hb.Reason, "already") {
		t.Fatalf("the hand-back is %+v", hb)
	}
}

// pushOnTop pushes one commit on top of the fixture's main to ref, as an
// earlier lease of the job would have, and answers its sha.
func pushOnTop(t *testing.T, g *rig, ref string) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", g.github.bare, dir).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	sha := commitIn(t, dir, "an earlier lease's")
	if out, err := sessionGit(dir, "push", "-q", "origin", "HEAD:"+ref); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	return sha
}

// A retried write lease starts from what an earlier lease of its job
// pushed, so its own push is a fast-forward: the most recent of the
// brief's pushed (Mesh's record of the earlier leases' ok pushes), else —
// from a Mesh that names none — the job's push_to. The first lease, and a
// lease whose pushed ref is gone, start from the base; the prompt says
// where the checkout starts.
func TestARetriedLeaseBuildsOnWhatAnEarlierLeasePushed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		leaseN  int
		pushTo  string
		pushed  []meshapi.BriefPushed // the earlier push's sha is filled in
		resumed bool                  // HEAD is the earlier push
		says    string
	}{
		{"the first lease", 1, "mesh/j-1", nil, false, ""},
		{"a retry named by push_to", 2, "mesh/j-1", nil, true, "An earlier run of this job already pushed mesh/j-1"},
		{"a retry named by the brief's pushed", 2, "", []meshapi.BriefPushed{{Ref: "refs/pins/old", LeaseN: 1}, {Ref: "refs/heads/mesh/j-1", LeaseN: 1}}, true,
			"An earlier run of this job (lease 1) already pushed mesh/j-1"},
		{"a pushed ref that is gone", 3, "", []meshapi.BriefPushed{{Ref: "refs/heads/mesh/gone", LeaseN: 2}}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var head, prompt string
			sess := &stubSession{result: SessionResult{SessionID: "sess-retry"}}
			sess.inspect = func(spec SessionSpec) {
				out, _ := sessionGit(spec.Workspace, "rev-parse", "HEAD")
				head, prompt = strings.TrimSpace(out), spec.Prompt
				commitIn(t, spec.Workspace, "this lease's")
				if text, refused, err := callTool(context.Background(), spec.Servers[NodeToolsServer], PushTool, map[string]any{"ref": "mesh/j-1"}); err != nil || refused != !tc.resumed {
					t.Errorf("the push: refused %v %s %v", refused, text, err)
				}
			}
			g := newRig(t, sess)
			earlier := pushOnTop(t, g, "refs/heads/mesh/j-1")
			g.offer(t, func(b *meshapi.Brief) {
				writeBrief(b)
				b.LeaseN = tc.leaseN
				b.Code.PushTo = tc.pushTo
				for _, p := range tc.pushed {
					p.SHA = earlier
					b.Pushed = append(b.Pushed, p)
				}
			})
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			want := g.github.head
			if tc.resumed {
				want = earlier
			}
			if head != want {
				t.Fatalf("the checkout is at %s, want %s", head, want)
			}
			if tc.says != "" && !strings.Contains(prompt, tc.says+", at "+earlier[:12]) {
				t.Fatalf("the prompt does not say %q:\n%s", tc.says, prompt)
			}
			if tc.says == "" && strings.Contains(prompt, "already pushed") {
				t.Fatalf("the prompt says of an earlier push:\n%s", prompt)
			}
		})
	}
}

// The relay's record of a push names each ref it updated and the
// commit, which Mesh hands to the job's next lease as its brief's
// pushed.
func TestAPushIsRecordedWithItsCommit(t *testing.T) {
	var pushed string
	sess := &stubSession{result: SessionResult{SessionID: "s"}}
	sess.inspect = func(spec SessionSpec) {
		pushed = commitIn(t, spec.Workspace, "two")
		if text, refused, err := callTool(context.Background(), spec.Servers[NodeToolsServer], PushTool, map[string]any{"ref": "mesh/j-1"}); err != nil || refused {
			t.Errorf("the push: %s %v", text, err)
		}
	}
	g := newRig(t, sess)
	g.offer(t, writeBrief)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	var access meshapi.AccessRequest
	json.Unmarshal(g.fake.Calls("access")[0].Body, &access)
	for _, rec := range access.Records {
		if rec.Service == "receive-pack" && rec.Outcome == "ok" && len(rec.Refs) > 0 {
			if len(rec.Pushed) != 1 || rec.Pushed[0] != (meshapi.PushedRef{Ref: "refs/heads/mesh/j-1", SHA: pushed}) {
				t.Fatalf("the push is recorded as %+v", rec)
			}
			return
		}
	}
	t.Fatalf("no ok push in %+v", access.Records)
}

// Two nodes of one OS user holding a lease of the same id — Mesh's
// derived lease ids are the same in every database, as two e2e runs on
// one laptop have them — never share its socket directory, and one
// releasing the lease leaves the other's alone.
func TestTwoNodesNeverShareALeaseSocket(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "mv1s-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	a := &Runtime{SocketRoot: root, WorkRoot: t.TempDir(), nodeID: "node-a"}
	b := &Runtime{SocketRoot: root, WorkRoot: t.TempDir(), nodeID: "node-b"}
	da, err := a.leaseSocketDir("lease-1")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := a.leaseSocketDir("lease-1"); again != da {
		t.Fatalf("one node's directory for a lease moved: %s, %s", da, again)
	}
	db, err := b.leaseSocketDir("lease-1")
	if err != nil {
		t.Fatal(err)
	}
	if da == db {
		t.Fatalf("both nodes have %s", da)
	}
	os.RemoveAll(db)
	if _, err := os.Stat(da); err != nil {
		t.Fatalf("the other node's release removed this one's directory: %v", err)
	}
	if filepath.Dir(da) != root || len(filepath.Base(da)) != leaseDirLen {
		t.Fatalf("the directory is %s", da)
	}
}
