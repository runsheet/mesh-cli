package nodev1

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/githarden"
	"projectmesh.io/mesh-cli/internal/githarden/githardentest"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A retried write lease's checkout moves onto what an earlier lease
// pushed with the git checkout runs (leaseGitRun): fetch, merge-base,
// checkout and rev-parse in a checkout a session planted a hook,
// fsmonitor, hooks path, credential helper and ssh command in run none
// of them on the host — neither when the fetch lands, nor when origin
// asks for credentials or is reached over ssh (mesh-specs gap 7).
func TestCheckoutsGitRunsNothingTheSessionPlanted(t *testing.T) {
	src := t.TempDir()
	githardentest.Git(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\n"), 0o644)
	githardentest.Git(t, src, "add", "-A")
	githardentest.Git(t, src, "commit", "-qm", "one")
	base := githardentest.Git(t, src, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("two\n"), 0o644)
	githardentest.Git(t, src, "commit", "-qam", "two")
	pushed := githardentest.Git(t, src, "rev-parse", "HEAD")
	githardentest.Git(t, src, "branch", "mesh/j-1")
	origin := filepath.Join(t.TempDir(), "origin.git")
	githardentest.Git(t, t.TempDir(), "clone", "-q", "--bare", src, origin)

	ws := filepath.Join(t.TempDir(), "work")
	githardentest.Git(t, filepath.Dir(ws), "clone", "-q", origin, ws)
	githardentest.Git(t, ws, "checkout", "-q", "--detach", base)
	p := githardentest.New(t, ws)

	ctx := context.Background()
	b := &meshapi.Brief{LeaseID: "lease-2", LeaseN: 2, Git: &meshapi.Git{Repo: "acme/fixture", Access: "write"},
		Code: &meshapi.Code{BaseSHA: base}, Pushed: []meshapi.BriefPushed{{Ref: "mesh/j-1", SHA: pushed, LeaseN: 1}}}
	at, err := resumePushed(leaseGitRun(ctx), b, ws, t.Logf)
	if err != nil || at == nil || at.SHA != pushed {
		t.Fatalf("the retried lease did not move onto its push: %+v %v", at, err)
	}
	for _, url := range []string{githardentest.AskingServer(t), githardentest.SSHRemote} {
		githardentest.Git(t, ws, "remote", "set-url", "origin", url)
		if at, err := resumePushed(leaseGitRun(ctx), b, ws, t.Logf); at != nil || err != nil {
			t.Fatalf("%s: a fetch that cannot land moved the checkout: %+v %v", url, at, err)
		}
	}
	p.NoneRan(t, "the lease's checkout")
}

// hardenedCommit commits in the session's checkout skipping what the
// session planted there: a session's own git running its own hooks is
// the session's business, so the test's session skips them and any
// marker left is the node's.
func hardenedCommit(t *testing.T, dir, msg string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "work.txt"), []byte(msg+"\n"), 0o644)
	id := []string{"GIT_AUTHOR_NAME=s", "GIT_AUTHOR_EMAIL=s@x", "GIT_COMMITTER_NAME=s", "GIT_COMMITTER_EMAIL=s@x"}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", msg}} {
		if out, err := githarden.Command(context.Background(), dir, id, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// A thread whose session planted a hook, fsmonitor, hooks path,
// credential helper and ssh command in its checkout runs none of them on
// the host (mesh-specs gap 7): not when the push tool pushes and the
// relay proves the fast-forward in that checkout, not when the parked
// checkout is reattached to the next lease's grant, and not on that
// lease's push.
func TestAThreadsPlantedCheckoutRunsNothingOnTheHost(t *testing.T) {
	var plant *githardentest.Plant
	var answers []string
	results := []SessionResult{{SessionID: "sess-1", Turns: 1, Parked: true}, {SessionID: "sess-1", Turns: 2}}
	n := 0
	sess := &stubSession{}
	sess.inspect = func(spec SessionSpec) {
		ctx := context.Background()
		sess.result = results[n]
		n++
		if plant == nil {
			commitIn(t, spec.Workspace, "one")
			plant = githardentest.New(t, spec.Workspace)
		} else {
			hardenedCommit(t, spec.Workspace, "two")
		}
		text, refused, err := callTool(ctx, spec.Servers[NodeToolsServer], PushTool, map[string]any{"ref": "mesh/j-1"})
		answers = append(answers, text)
		if refused || err != nil {
			t.Errorf("lease %d's push: %s %v", n, text, err)
		}
	}
	g := newRig(t, sess)
	g.offer(t, func(b *meshapi.Brief) {
		writeBrief(b)
		threadOffer("thread_1", "", &meshapi.Turn{TurnID: "t-1", Seq: 1})(b)
	})
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	first := g.github.ref("refs/heads/mesh/j-1")
	g.offer(t, func(b *meshapi.Brief) {
		writeBrief(b)
		threadOffer("thread_1", "sess-1", &meshapi.Turn{TurnID: "t-2", Seq: 2})(b)
	})
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if n != 2 || first == "" || g.github.ref("refs/heads/mesh/j-1") == first {
		t.Fatalf("the thread's two leases did not each push: %d leases, %q then %q (%v)", n, first, g.github.ref("refs/heads/mesh/j-1"), answers)
	}
	plant.NoneRan(t, "the thread's leases")
}
