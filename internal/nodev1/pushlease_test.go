package nodev1

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// bareCommit makes a commit in the fake vendor's repository on top of
// parent, with parent's tree, and answers it: a push someone else made.
func bareCommit(t *testing.T, bare, parent, msg string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", bare, "commit-tree", parent+"^{tree}", "-p", parent, "-m", msg)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=o", "GIT_AUTHOR_EMAIL=o@x", "GIT_COMMITTER_NAME=o", "GIT_COMMITTER_EMAIL=o@x")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("commit-tree: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func bareSet(t *testing.T, bare, ref, sha string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", bare, "update-ref", ref, sha).CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}
}

// A push takes a lease (mesh-specs gaps, 35): a rebase job's rebased
// push_to is no fast-forward, so a plain push is refused, as before, and
// says a lease would do; a leased push replaces push_to from where the
// job found it; an ordinary push still fast-forwards; once someone else
// pushes, a lease is refused naming where the branch is now and nothing
// is overwritten; a forced push without a lease is refused as ever; and
// a ref other than push_to takes no lease.
func TestALeasedPushReplacesPushToOnlyWhereTheJobFoundIt(t *testing.T) {
	const branch = "refs/heads/mesh/rebase"
	var found, rebased, more, theirs string
	type answer struct {
		name, text string
		refused    bool
	}
	var answers []answer
	sess := &stubSession{calls: []string{"conform/submit"}, result: SessionResult{SessionID: "sess-lease", Text: "pushed"}}
	var g *rig
	sess.inspect = func(spec SessionSpec) {
		ctx := context.Background()
		push := func(name string, args map[string]any) {
			text, refused, err := callTool(ctx, spec.Servers[NodeToolsServer], PushTool, args)
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			answers = append(answers, answer{name, text, refused})
		}
		rebased = commitIn(t, spec.Workspace, "rebased")
		push("plain", map[string]any{})
		push("leased", map[string]any{"lease": "head"})
		more = commitIn(t, spec.Workspace, "more")
		push("fast-forward", map[string]any{})
		theirs = bareCommit(t, g.github.bare, more, "theirs")
		bareSet(t, g.github.bare, branch, theirs)
		sessionGit(spec.Workspace, "reset", "-q", "--hard", "HEAD~2")
		commitIn(t, spec.Workspace, "rebased again")
		push("moved", map[string]any{"lease": "head"})
		out, err := sessionGit(spec.Workspace, "push", "--force", "origin", "HEAD:"+branch)
		answers = append(answers, answer{"forced", out, err != nil})
		push("other ref", map[string]any{"ref": "mesh/other", "lease": "head"})
	}
	g = newRig(t, sess)
	found = bareCommit(t, g.github.bare, g.github.head, "the branch as the job found it")
	bareSet(t, g.github.bare, branch, found)
	job := g.offer(t, func(b *meshapi.Brief) {
		writeBrief(b)
		b.Code = &meshapi.Code{Repo: meshapi.Repo{Host: "github.com", FullName: "acme/fixture"}, BaseRef: "main", BaseSHA: g.github.head, PushTo: "mesh/rebase"}
	})
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if lease := g.fake.LeaseOf(job); g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q", lease, g.fake.Ended(lease))
	}
	want := map[string]struct {
		refused bool
		says    string
	}{
		"plain":        {true, `lease "head"`},
		"leased":       {false, `"lease":"` + found + `"`},
		"fast-forward": {false, `"sha":"` + more},
		"moved":        {true, "is at " + theirs + " now, not " + more},
		"forced":       {true, "403"},
		"other ref":    {true, "takes no lease"},
	}
	if len(answers) != len(want) {
		t.Fatalf("answers %+v", answers)
	}
	for _, a := range answers {
		w := want[a.name]
		if a.refused != w.refused || !strings.Contains(a.text, w.says) {
			t.Errorf("%s: refused %v, %q; want refused %v saying %q", a.name, a.refused, a.text, w.refused, w.says)
		}
	}
	if rebased == "" {
		t.Fatal("no rebased commit")
	}
	if got := g.github.ref(branch); got != theirs {
		t.Errorf("%s is %s, want their push %s, never overwritten", branch, got, theirs)
	}
	if got := g.github.ref("refs/heads/mesh/other"); got != "" {
		t.Errorf("mesh/other was pushed: %s", got)
	}
	// the lease is the leased push's AccessRecord's reason
	// and the forced push's, why the relay refused it
	var access string
	for _, c := range g.fake.Calls("access") {
		access += string(c.Body)
	}
	if !strings.Contains(access, "leased: "+branch+" from "+found) {
		t.Error("no AccessRecord names the lease")
	}
	if !strings.Contains(access, branch+" may only fast-forward") {
		t.Error("no AccessRecord refuses the forced push")
	}
}
