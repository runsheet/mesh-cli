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

// A retried write lease of a job naming push_to starts from what an
// earlier lease pushed there, so its own push is a fast-forward; the
// first lease starts from the base whatever push_to holds.
func TestARetriedLeaseBuildsOnWhatAnEarlierLeasePushed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		leaseN int
		pushed bool // HEAD is the earlier push
	}{
		{"the first lease", 1, false},
		{"a retried lease", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var head, prompt string
			sess := &stubSession{result: SessionResult{SessionID: "sess-retry"}}
			sess.inspect = func(spec SessionSpec) {
				out, _ := sessionGit(spec.Workspace, "rev-parse", "HEAD")
				head, prompt = strings.TrimSpace(out), spec.Prompt
				commitIn(t, spec.Workspace, "this lease's")
				if text, refused, err := callTool(context.Background(), spec.Servers[NodeToolsServer], PushTool, map[string]any{"ref": "mesh/j-1"}); err != nil || refused != !tc.pushed {
					t.Errorf("the push: refused %v %s %v", refused, text, err)
				}
			}
			g := newRig(t, sess)
			earlier := pushOnTop(t, g, "refs/heads/mesh/j-1")
			g.offer(t, func(b *meshapi.Brief) {
				writeBrief(b)
				b.LeaseN = tc.leaseN
				b.Code.PushTo = "mesh/j-1"
			})
			if err := run(t, g, context.Background()); err != nil {
				t.Fatal(err)
			}
			want := g.github.head
			if tc.pushed {
				want = earlier
			}
			if head != want {
				t.Fatalf("the checkout is at %s, want %s", head, want)
			}
			if says := strings.Contains(prompt, "already pushed mesh/j-1"); says != (tc.leaseN > 1) {
				t.Fatalf("the prompt says of the earlier push %v:\n%s", says, prompt)
			}
		})
	}
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
