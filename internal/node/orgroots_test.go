package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// Two orgs' roots never meet: every directory of one is outside every
// directory of the other, and all of them under ~/.mesh/orgs/<org>/.
func TestOrgRootsForTwoOrgsAreDisjoint(t *testing.T) {
	mesh := filepath.Join(t.TempDir(), ".mesh")
	set, err := NewOrgRootSet(mesh, []string{"acme", "grapevine"})
	if err != nil {
		t.Fatal(err)
	}
	if set.Single() {
		t.Fatal("two orgs laid out as one")
	}
	a, err := set.For("acme")
	if err != nil {
		t.Fatal(err)
	}
	b, err := set.For("grapevine")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"acme": a.Base, "grapevine": b.Base}
	for org, base := range want {
		if base != filepath.Join(mesh, "orgs", org) {
			t.Fatalf("%s base = %s, want %s", org, base, filepath.Join(mesh, "orgs", org))
		}
	}
	names := []string{"Work", "Repos", "Scratch", "Store", "Kept", "Resumes", "MCP"}
	for i, d := range a.Dirs() {
		if d == "" || b.Dirs()[i] == "" {
			t.Fatalf("%s empty: %q, %q", names[i], d, b.Dirs()[i])
		}
		if !under(d, a.Base) || !under(b.Dirs()[i], b.Base) {
			t.Fatalf("%s outside its org: %s, %s", names[i], d, b.Dirs()[i])
		}
		for j, e := range b.Dirs() {
			if under(d, e) || under(e, d) {
				t.Fatalf("acme's %s %s meets grapevine's %s %s", names[i], d, names[j], e)
			}
		}
	}
	// an org it does not serve is refused, never another's roots
	if _, err := set.For("other"); err == nil || !strings.Contains(err.Error(), `"other"`) {
		t.Fatalf("For(other) err = %v, want a refusal naming it", err)
	}
}

// A node serving one org keeps today's paths: nothing moves.
func TestOrgRootsForOneOrgKeepTodaysPaths(t *testing.T) {
	mesh := filepath.Join(t.TempDir(), ".mesh")
	set, err := NewOrgRootSet(mesh, []string{"acme"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := set.For("acme")
	if err != nil {
		t.Fatal(err)
	}
	if !set.Single() || r.Work != filepath.Join(mesh, "work") || r.Repos != filepath.Join(mesh, "repos") || r.Store != filepath.Join(mesh, "store") || r.Scratch != filepath.Join(mesh, "work", ScratchDirName) {
		t.Fatalf("one org's roots = %+v, want today's under %s", r, mesh)
	}
}

// An org name that is not one path segment, or that differs only in case
// from another served org, is refused with an error naming it.
func TestOrgRootsRefuseABadOrgName(t *testing.T) {
	mesh := t.TempDir()
	for _, tc := range []struct {
		orgs []string
		name string
	}{
		{[]string{"acme", ""}, `""`},
		{[]string{"acme", "."}, `"."`},
		{[]string{"acme", ".."}, `".."`},
		{[]string{"acme", "a/b"}, `"a/b"`},
		{[]string{"acme", `a\b`}, `"a\\b"`},
		{[]string{"acme", "/abs"}, `"/abs"`},
		{[]string{".."}, `".."`},
		{[]string{"Acme", "acme"}, `"acme"`},
		{[]string{"acme", "ACME"}, `"ACME"`},
	} {
		_, err := NewOrgRootSet(mesh, tc.orgs)
		if err == nil {
			t.Fatalf("%q: no refusal", tc.orgs)
		}
		if tc.name == `""` {
			if !strings.Contains(err.Error(), "empty org name") {
				t.Fatalf("%q: err = %v, want it to name the empty org", tc.orgs, err)
			}
			continue
		}
		if !strings.Contains(err.Error(), tc.name) {
			t.Fatalf("%q: err = %v, want it to name %s", tc.orgs, err, tc.name)
		}
	}
	if _, err := RootsForOrg(mesh, ".."); err == nil {
		t.Fatal("RootsForOrg(..) not refused")
	}
	if _, err := NewOrgRootSet(mesh, nil); err == nil {
		t.Fatal("no org not refused")
	}
}

// The same repo prepared for two orgs is two shared clones, each under
// its own org's repos root, and each org's worktree is added from its own
// org's clone alone — never the other's, and never Git.ReposRoot's.
func TestPrepareSameRepoUnderTwoOrgsMakesTwoClones(t *testing.T) {
	root, origin := lockTestOrigin(t)
	mesh := filepath.Join(root, ".mesh")
	scopes := Scopes{{Login: "acme", ID: "org-a"}, {Login: "grapevine", ID: "org-b"}}
	set, err := NewOrgRootSet(mesh, []string{"acme", "grapevine"})
	if err != nil {
		t.Fatal(err)
	}
	type call struct{ dir, cmd string }
	var mu sync.Mutex
	var calls []call
	nodeWide := filepath.Join(mesh, "repos")
	g := &Git{ReposRoot: nodeWide}
	g.exec = func(ctx context.Context, dir, name string, args ...string) (string, error) {
		mu.Lock()
		calls = append(calls, call{dir, name + " " + strings.Join(args, " ")})
		mu.Unlock()
		return g.realRun(ctx, dir, name, args...)
	}
	clones := map[string]string{}
	for _, org := range []string{"org-a", "org-b"} {
		spec, err := orgSpec(set, scopes, org, WorkspaceSpec{Root: filepath.Join(mesh, "work"), GoalSlug: "g", Seq: 1, Repo: origin, Branch: "mesh/g-1", BaseRef: "main"})
		if err != nil {
			t.Fatal(err)
		}
		dir, err := g.Prepare(context.Background(), spec)
		if err != nil {
			t.Fatalf("prepare %s: %v", org, err)
		}
		sc, _ := scopes.ByOrg(org)
		r, _ := set.For(sc.Name())
		if !under(dir, r.Work) {
			t.Fatalf("%s checkout %s not under its work root %s", org, dir, r.Work)
		}
		clone := g.sharedClone(spec)
		if clone != SharedClonePath(r.Repos, origin) {
			t.Fatalf("%s clone = %s, want under %s", org, clone, r.Repos)
		}
		if got := worktreeClone(dir); got != "" && filepath.Clean(got) != filepath.Clean(clone) {
			t.Fatalf("%s worktree's clone = %s, want %s", org, got, clone)
		}
		if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
			t.Fatalf("%s clone %s not made: %v", org, clone, err)
		}
		clones[org] = clone
	}
	if clones["org-a"] == clones["org-b"] {
		t.Fatalf("one clone for two orgs: %s", clones["org-a"])
	}
	if _, err := os.Stat(nodeWide); err == nil {
		t.Fatalf("the node-wide repos root %s was used", nodeWide)
	}
	added := map[string]int{}
	for _, c := range calls {
		if !strings.HasPrefix(c.cmd, "git worktree add") {
			continue
		}
		for org, clone := range clones {
			if filepath.Clean(c.dir) != filepath.Clean(clone) {
				continue
			}
			added[org]++
			other := "org-a"
			if org == "org-a" {
				other = "org-b"
			}
			sc, _ := scopes.ByOrg(other)
			r, _ := set.For(sc.Name())
			if strings.Contains(c.cmd, r.Base) {
				t.Fatalf("%s's clone added a worktree in %s's tree: %s", org, other, c.cmd)
			}
		}
		if !strings.HasPrefix(c.dir, clones["org-a"]) && !strings.HasPrefix(c.dir, clones["org-b"]) {
			t.Fatalf("worktree added from %s, not an org's clone", c.dir)
		}
	}
	if added["org-a"] != 1 || added["org-b"] != 1 {
		t.Fatalf("worktree adds per org clone = %v, want one each; calls %v", added, calls)
	}
}

// under says p is d or inside it.
func under(p, d string) bool {
	rel, err := filepath.Rel(d, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// An item of an org the node does not serve gets no roots — never
// another org's — and a dir under an org's work root finds that org's
// repos root whatever Git.ReposRoot says.
func TestOrgSpecRefusesAnUnservedOrgAndFindsTheOrgsRepos(t *testing.T) {
	mesh := filepath.Join(t.TempDir(), ".mesh")
	scopes := Scopes{{Login: "acme", ID: "org-a"}, {Slug: "grapevine", ID: "org-b"}}
	set, err := NewOrgRootSet(mesh, []string{"acme", "grapevine"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orgSpec(set, scopes, "org-z", WorkspaceSpec{}); err == nil || !strings.Contains(err.Error(), `"org-z"`) {
		t.Fatalf("orgSpec(org-z) err = %v, want a refusal naming it", err)
	}
	if _, err := scratchRootFor(set, scopes, "org-z", ""); err == nil {
		t.Fatal("scratchRootFor(org-z) not refused")
	}
	if _, err := workRootOfScope(set, Scope{Login: "other"}, ""); err == nil {
		t.Fatal("workRootOfScope(other) not refused")
	}
	b, _ := set.For("grapevine")
	if s, _ := scratchRootFor(set, scopes, "org-b", ""); s != b.Scratch {
		t.Fatalf("org-b scratch = %s, want %s", s, b.Scratch)
	}
	g := &Git{ReposRoot: filepath.Join(mesh, "repos")}
	if got := g.reposRootFor(filepath.Join(b.Work, "g", "1")); got != b.Repos {
		t.Fatalf("reposRootFor = %s, want %s", got, b.Repos)
	}
	if got := g.reposRootFor(filepath.Join(mesh, "work", "g", "1")); got != g.ReposRoot {
		t.Fatalf("reposRootFor today's = %s, want %s", got, g.ReposRoot)
	}
	// with no roots laid out, today's root stands
	if spec, err := orgSpec(nil, scopes, "org-z", WorkspaceSpec{Root: "/w"}); err != nil || spec.Root != "/w" || spec.Repos != "" {
		t.Fatalf("orgSpec(nil) = %+v, %v", spec, err)
	}
}

// The kept-sessions sweep, given two orgs' roots, removes an expired
// kept session under each org's Kept root and keeps a young one in each.
func TestKeptSessionsSweepCoversEveryOrgsKeptRoot(t *testing.T) {
	set, err := NewOrgRootSet(t.TempDir(), []string{"acme", "grapevine"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	mk := func(kept, name string, started time.Time) string {
		dir := filepath.Join(kept, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		note := fmt.Sprintf(`{"kind":"evaluate","startedAt":%q}`, started.Format(time.RFC3339Nano))
		if err := os.WriteFile(filepath.Join(dir, sessionunit.StateFile), []byte(note), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	var old, young []string
	for _, org := range set.Orgs() {
		r, _ := set.For(org)
		old = append(old, mk(r.Kept, "old-1", now.Add(-25*time.Hour)))
		young = append(young, mk(r.Kept, "young-1", now.Add(-time.Hour)))
	}
	if err := set.SweepKeptSessions(now); err != nil {
		t.Fatal(err)
	}
	for _, d := range old {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("expired %s kept (err %v)", d, err)
		}
	}
	for _, d := range young {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("young %s removed: %v", d, err)
		}
	}
}

// keptRootFor answers the item's org's Kept root, refuses an org the
// node does not serve, and is "" (today's path) with no roots laid out.
func TestKeptRootForIsTheItemsOrg(t *testing.T) {
	mesh := t.TempDir()
	set, err := NewOrgRootSet(mesh, []string{"acme", "grapevine"})
	if err != nil {
		t.Fatal(err)
	}
	scopes := Scopes{{Login: "acme", ID: "org-a"}, {Login: "grapevine", ID: "org-b"}}
	got, err := keptRootFor(set, scopes, "org-a")
	if want := filepath.Join(mesh, "orgs", "acme", "work", "kept-sessions"); err != nil || got != want {
		t.Fatalf("keptRootFor(org-a) = %q, %v; want %q", got, err, want)
	}
	if _, err := keptRootFor(set, scopes, "org-x"); err == nil {
		t.Fatal("unserved org: want a refusal")
	}
	if got, err := keptRootOfScope(set, Scope{Login: "grapevine"}); err != nil || !strings.HasPrefix(got, filepath.Join(mesh, "orgs", "grapevine")) {
		t.Fatalf("keptRootOfScope(grapevine) = %q, %v", got, err)
	}
	if got, err := keptRootFor(nil, scopes, "org-a"); err != nil || got != "" {
		t.Fatalf("no roots: %q, %v; want \"\"", got, err)
	}
}
