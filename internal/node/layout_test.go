package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A job opened for a served org's item makes its directory and job.json
// under ~/.mesh/jobs/<id>, naming its connection; nothing else under
// ~/.mesh, and no path names the org or the product (ADR-039).
func TestOpenJobLaysOutTheJob(t *testing.T) {
	l := testLayout(t)
	scopes := connected(Scopes{{Login: "acme", ID: "org-a"}})
	j, err := openJob(l, scopes, "org-a", JobMeta{ID: "item-1", Unit: JobItem, ItemID: "item-1", GoalID: "goal-1", Label: "ship-it/1"})
	if err != nil {
		t.Fatal(err)
	}
	if j.Dir != filepath.Join(l.Dir, "jobs", "item-1") || j.Store != filepath.Join(j.Dir, "store") {
		t.Fatalf("job %s, store %s", j.Dir, j.Store)
	}
	if entries, _ := os.ReadDir(l.Dir); len(entries) != 1 || entries[0].Name() != "jobs" {
		t.Fatalf("~/.mesh holds %v, want the jobs tree alone", entries)
	}
	m, err := ReadJob(j.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "item-1" || m.Unit != JobItem || m.ConnectionID != "conn-acme" || m.OrgID != "org-a" || m.Product != "runsheet" || m.Started.IsZero() {
		t.Fatalf("job.json = %+v", m)
	}
	for _, p := range []string{j.Dir, j.Checkout(), j.Kept, j.MCP, j.Store} {
		if strings.Contains(p, "acme") {
			t.Errorf("%s names the org", p)
		}
	}

	// opened again, the same job keeps its job.json
	again, err := openJob(l, scopes, "org-a", JobMeta{ID: "item-1", Unit: JobItem, ItemID: "item-1"})
	if err != nil || again.Dir != j.Dir {
		t.Fatalf("reopen: %s, %v", again.Dir, err)
	}
	if m2, _ := ReadJob(j.Dir); m2.Label != "ship-it/1" || !m2.Started.Equal(m.Started) {
		t.Fatalf("reopened job.json rewritten: %+v", m2)
	}
}

// A job never moves between connections, and a served org with no
// connection on the node's answer, or an org not served, opens nothing.
func TestOpenJobRefusesAnotherConnection(t *testing.T) {
	l := testLayout(t)
	two := connected(Scopes{{Login: "acme", ID: "org-a"}, {Login: "beta", ID: "org-b"}})
	if _, err := openJob(l, two, "org-a", JobMeta{ID: "item-1", Unit: JobItem}); err != nil {
		t.Fatal(err)
	}
	if _, err := openJob(l, two, "org-b", JobMeta{ID: "item-1", Unit: JobItem}); err == nil {
		t.Fatal("a job was opened again for another connection")
	}
	if _, err := openJob(l, Scopes{{Login: "acme", ID: "org-a"}}, "org-a", JobMeta{ID: "item-2", Unit: JobItem}); err == nil {
		t.Fatal("a scope with no connection opened a job")
	}
	if _, err := openJob(l, two, "org-c", JobMeta{ID: "item-3", Unit: JobItem}); err == nil {
		t.Fatal("an unserved org opened a job")
	}
	for _, key := range []string{"", ".", "..", "a/b", `a\b`, " a"} {
		if _, err := l.Job(key); err == nil {
			t.Errorf("job key %q accepted", key)
		}
	}
}

// An item's spec in its job checks out <job>/checkout as a clone of its
// own — no shared clone, no repos root — under the job's own locks; its
// evaluation and rebase are <job>/eval and <job>/rebase, clones too.
func TestItemSpecIsInItsJob(t *testing.T) {
	l := testLayout(t)
	scopes := connected(Scopes{{Login: "acme", ID: "org-a"}})
	item := testItem("item-1", "org-a")
	spec, err := itemSpec(l, scopes, item, "ship-it/1", WorkspaceSpec{Root: "/unused", GoalSlug: "ship-it", Seq: 1, Repo: "acme/widgets", Branch: "mesh/ship-it-1", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := l.Job("item-1")
	if spec.Dir() != j.Checkout() || spec.Repos != "" || spec.Root != j.Dir {
		t.Fatalf("spec = %+v (dir %s)", spec, spec.Dir())
	}
	g := &Git{}
	if clone := g.seedClone(spec); clone != "" {
		t.Fatalf("a job's checkout is seeded from %s", clone)
	}
	if r := RebaseWorkspaceSpec(spec); r.Dir() != j.Rebase() || !isRebaseDir(r.Dir()) {
		t.Fatalf("rebase dir %s", r.Dir())
	}
	ev, err := evalSpec(l, scopes, item, "ship-it/1", EvalWorkspaceSpec("/unused", "ship-it", 1, "acme/widgets", "mesh/ship-it-1", "main"))
	if err != nil || ev.Dir() != j.Eval() {
		t.Fatalf("eval dir %s, %v", ev.Dir(), err)
	}
	for _, dir := range []string{j.Checkout(), j.Eval(), j.Rebase(), j.Attempt("candidate", 1)} {
		if got := g.lockRoot("", dir); got != j.Dir {
			t.Errorf("lock root of %s = %s, want the job's %s", dir, got, j.Dir)
		}
		if got := g.reposRootFor(dir); got != "" {
			t.Errorf("repos root of %s = %s, want none", dir, got)
		}
	}
	// the clone command takes the token from the environment, never the
	// command line
	cmd := g.cloneCommand(withGitToken(context.Background(), GitHubURL, &gateway.GitHubToken{Token: "ghs_" + strings.Repeat("a", 36)}), "acme/widgets", j.Checkout())
	if cmd[0] != "git" || strings.Contains(strings.Join(cmd, " "), "ghs_") {
		t.Fatalf("clone command %q", cmd)
	}
}
