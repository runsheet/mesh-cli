package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A job opened for a served org's item makes its directory and job.json
// under ~/.mesh/jobs/<id>, and its connection's caches and
// connection.json under ~/.mesh/connections/<connection>; no path names
// the org or the product (ADR-039).
func TestOpenJobLaysOutTheJobAndItsConnection(t *testing.T) {
	l := testLayout(t)
	scopes := connected(Scopes{{Login: "acme", ID: "org-a"}})
	j, c, err := openJob(l, scopes, "org-a", JobMeta{ID: "item-1", Unit: JobItem, ItemID: "item-1", GoalID: "goal-1", Label: "ship-it/1"})
	if err != nil {
		t.Fatal(err)
	}
	if j.Dir != filepath.Join(l.Dir, "jobs", "item-1") || c.Dir != filepath.Join(l.Dir, "connections", "conn-acme") {
		t.Fatalf("job %s, connection %s", j.Dir, c.Dir)
	}
	m, err := ReadJob(j.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "item-1" || m.Unit != JobItem || m.ConnectionID != "conn-acme" || m.OrgID != "org-a" || m.Product != "runsheet" || m.Started.IsZero() {
		t.Fatalf("job.json = %+v", m)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, ConnectionFile)); err != nil {
		t.Fatalf("connection.json: %v", err)
	}
	for _, p := range []string{j.Dir, j.Checkout(), j.Kept, j.MCP, c.Repos, c.Store} {
		if strings.Contains(p, "acme") && !strings.Contains(p, "conn-acme") {
			t.Errorf("%s names the org", p)
		}
	}

	// opened again, the same job keeps its job.json
	again, _, err := openJob(l, scopes, "org-a", JobMeta{ID: "item-1", Unit: JobItem, ItemID: "item-1"})
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
	if _, _, err := openJob(l, two, "org-a", JobMeta{ID: "item-1", Unit: JobItem}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openJob(l, two, "org-b", JobMeta{ID: "item-1", Unit: JobItem}); err == nil {
		t.Fatal("a job was opened again for another connection")
	}
	if _, _, err := openJob(l, Scopes{{Login: "acme", ID: "org-a"}}, "org-a", JobMeta{ID: "item-2", Unit: JobItem}); err == nil {
		t.Fatal("a scope with no connection opened a job")
	}
	if _, _, err := openJob(l, two, "org-c", JobMeta{ID: "item-3", Unit: JobItem}); err == nil {
		t.Fatal("an unserved org opened a job")
	}
	for _, key := range []string{"", ".", "..", "a/b", `a\b`, " a"} {
		if _, err := l.Job(key); err == nil {
			t.Errorf("job key %q accepted", key)
		}
		if _, err := l.Connection(key); err == nil {
			t.Errorf("connection key %q accepted", key)
		}
	}
}

// An item's spec in its job checks out <job>/checkout as a worktree of
// its connection's clone, under its connection's locks; its evaluation
// and rebase are <job>/eval and <job>/rebase; the repo lock and the
// repos root found from any of them are the connection's.
func TestItemSpecIsInItsJob(t *testing.T) {
	l := testLayout(t)
	scopes := connected(Scopes{{Login: "acme", ID: "org-a"}})
	item := testItem("item-1", "org-a")
	spec, err := itemSpec(l, scopes, item, "ship-it/1", WorkspaceSpec{Root: "/unused", GoalSlug: "ship-it", Seq: 1, Repo: "acme/widgets", Branch: "mesh/ship-it-1", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := l.Job("item-1")
	c, _ := l.Connection("conn-acme")
	if spec.Dir() != j.Checkout() || spec.Repos != c.Repos || spec.Root != c.Dir || spec.Path != "" {
		t.Fatalf("spec = %+v (dir %s)", spec, spec.Dir())
	}
	g := &Git{}
	if clone := g.sharedClone(spec); clone != SharedClonePath(c.Repos, "acme/widgets") {
		t.Fatalf("shared clone %s", clone)
	}
	if r := RebaseWorkspaceSpec(spec); r.Dir() != j.Rebase() || !isRebaseDir(r.Dir()) {
		t.Fatalf("rebase dir %s", r.Dir())
	}
	ev, err := evalSpec(l, scopes, item, "ship-it/1", EvalWorkspaceSpec("/unused", "ship-it", 1, "acme/widgets", "mesh/ship-it-1", "main"))
	if err != nil || ev.Dir() != j.Eval() {
		t.Fatalf("eval dir %s, %v", ev.Dir(), err)
	}
	for _, dir := range []string{j.Checkout(), j.Eval(), j.Rebase()} {
		if got := g.lockRoot("", dir); got != c.Dir {
			t.Errorf("lock root of %s = %s, want %s", dir, got, c.Dir)
		}
		if got := g.reposRootFor(dir); got != c.Repos {
			t.Errorf("repos root of %s = %s, want %s", dir, got, c.Repos)
		}
	}
	// a plan's full clones keep their own locks
	plan, err := planDirFor(l, scopes[0], testGoal("goal-1", "ship-it"), "/unused")
	if err != nil {
		t.Fatal(err)
	}
	if got := g.lockRoot("", filepath.Join(plan, "repos", "widgets")); got != plan {
		t.Errorf("a plan clone's lock root = %s, want its plan dir %s", got, plan)
	}
}
