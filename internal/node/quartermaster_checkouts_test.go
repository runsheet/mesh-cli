package node

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// checkoutsGateway answers the dead-checkout check's reads: one org, its
// goals, and each goal's items; err fails every read.
type checkoutsGateway struct {
	Gateway
	goals []gateway.Goal
	items map[string][]gateway.WorkItem
	err   error
}

func (g *checkoutsGateway) MyOrgs(context.Context) ([]gateway.Org, error) {
	if g.err != nil {
		return nil, g.err
	}
	return []gateway.Org{{ID: "org-1", Login: "acme"}}, nil
}

func (g *checkoutsGateway) Goals(context.Context, string) ([]gateway.Goal, error) {
	if g.err != nil {
		return nil, g.err
	}
	return g.goals, nil
}

func (g *checkoutsGateway) GoalWorkItems(_ context.Context, goalID string) ([]gateway.WorkItem, error) {
	if g.err != nil {
		return nil, g.err
	}
	return g.items[goalID], nil
}

// checkoutsFixture is a work root and a shared clone at
// <base>/repos/acme/widgets, with a worktree per seq of goal "g" under
// <base>/work/g/<seq>, each with a file of its own and aged two hours.
type checkoutsFixture struct {
	root, clone string
	git         *Git
}

func newCheckoutsFixture(t *testing.T, seqs ...string) checkoutsFixture {
	t.Helper()
	base := t.TempDir()
	origin := filepath.Join(base, "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, origin, "init", "-q")
	writeFiles(t, origin, map[string]string{"README.md": "hi\n"})
	gitRun(t, origin, "add", ".")
	gitRun(t, origin, "commit", "-qm", "init")
	clone := filepath.Join(base, "repos", "acme", "widgets")
	gitRun(t, base, "clone", "-q", origin, clone)
	root := filepath.Join(base, "work")
	for _, seq := range seqs {
		dir := filepath.Join(root, "g", seq)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		gitRun(t, clone, "worktree", "add", "-q", "-b", "mesh/g-"+seq, dir)
		writeFiles(t, dir, map[string]string{"build.out": strings.Repeat("x", 1000)})
		ageCheckout(t, dir, 2*time.Hour)
	}
	return checkoutsFixture{root: root, clone: clone, git: &Git{}}
}

// ageCheckout sets every mtime of dir's tree and its worktree admin dir
// to ago.
func ageCheckout(t *testing.T, dir string, ago time.Duration) {
	t.Helper()
	when := time.Now().Add(-ago)
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	admin := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
	for _, top := range []string{dir, admin} {
		err := filepath.WalkDir(top, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(p, when, when)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func (f checkoutsFixture) worktrees(t *testing.T) string {
	return gitRun(t, f.clone, "worktree", "list", "--porcelain")
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestDeadCheckoutCheckRemovesOnlyDeadIdleCheckouts(t *testing.T) {
	// 1 merged; 2 ready; 3 claimed; 4 cancelled but live; 5 merged but
	// touched within the hour; 6 unknown to the platform.
	f := newCheckoutsFixture(t, "1", "2", "3", "4", "5", "6")
	writeFiles(t, filepath.Join(f.root, "g", "5"), map[string]string{"fresh.txt": "now"})
	gw := &checkoutsGateway{
		goals: []gateway.Goal{{ID: "goal-g", Slug: "g"}},
		items: map[string][]gateway.WorkItem{"goal-g": {
			{ID: "i1", Seq: 1, Status: "merged"},
			{ID: "i2", Seq: 2, Status: "ready"},
			{ID: "i3", Seq: 3, Status: "claimed"},
			{ID: "i4", Seq: 4, Status: "cancelled"},
			{ID: "i5", Seq: 5, Status: "merged"},
		}},
	}
	dir := func(seq string) string { return filepath.Join(f.root, "g", seq) }
	want := dirBytes(dir("1")) + dirBytes(dir("6"))
	live := func(d string) bool { return d == dir("4") }

	got := DeadCheckoutCheck(gw, f.git, f.root, live)(context.Background(), time.Now())
	if len(got) != 1 {
		t.Fatalf("findings = %+v", got)
	}
	fd := got[0]
	t.Logf("%+v", fd)

	for _, seq := range []string{"1", "6"} {
		if exists(dir(seq)) {
			t.Errorf("g/%s kept, want removed", seq)
		}
		if strings.Contains(f.worktrees(t), dir(seq)+"\n") {
			t.Errorf("g/%s still in git worktree list:\n%s", seq, f.worktrees(t))
		}
	}
	for _, seq := range []string{"2", "3", "4", "5"} {
		if !exists(dir(seq)) {
			t.Errorf("g/%s removed, want kept", seq)
		}
	}
	if fd.Kind != "dead_checkout" || fd.Severity != "ok" {
		t.Errorf("kind/severity = %s/%s", fd.Kind, fd.Severity)
	}
	if fd.Reclaimed != want || want < 2000 {
		t.Errorf("Reclaimed = %d, want %d", fd.Reclaimed, want)
	}
	for _, s := range []string{"4 dead found", "2 removed", "1 kept because live", "1 kept as touched within 1h0m0s"} {
		if !strings.Contains(fd.Detail, s) {
			t.Errorf("detail %q lacks %q", fd.Detail, s)
		}
	}
}

func TestDeadCheckoutCheckKeepsEverythingWhenThePlatformCannotAnswer(t *testing.T) {
	f := newCheckoutsFixture(t, "1", "2")
	gw := &checkoutsGateway{err: errors.New("503 service unavailable")}

	got := DeadCheckoutCheck(gw, f.git, f.root, nil)(context.Background(), time.Now())
	fd := got[0]
	for _, seq := range []string{"1", "2"} {
		if !exists(filepath.Join(f.root, "g", seq)) {
			t.Errorf("g/%s removed during an outage", seq)
		}
	}
	if fd.Severity != "warn" || fd.Reclaimed != 0 || !strings.Contains(fd.Detail, "the platform could not answer for 2 checkouts") {
		t.Errorf("finding = %+v", fd)
	}
}

func TestDeadCheckoutCheckPrunesAWorktreeWhoseDirIsGone(t *testing.T) {
	f := newCheckoutsFixture(t, "1", "2")
	gone := filepath.Join(f.root, "g", "1")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.worktrees(t), "prunable") {
		t.Fatalf("no prunable worktree before the check:\n%s", f.worktrees(t))
	}
	gw := &checkoutsGateway{
		goals: []gateway.Goal{{ID: "goal-g", Slug: "g"}},
		items: map[string][]gateway.WorkItem{"goal-g": {{Seq: 1, Status: "merged"}, {Seq: 2, Status: "running"}}},
	}

	fd := DeadCheckoutCheck(gw, f.git, f.root, nil)(context.Background(), time.Now())[0]
	list := f.worktrees(t)
	if strings.Contains(list, gone) || strings.Contains(list, "prunable") {
		t.Errorf("gone worktree still listed:\n%s", list)
	}
	if !exists(filepath.Join(f.root, "g", "2")) {
		t.Error("g/2 removed, want kept")
	}
	if !strings.Contains(fd.Detail, "1 prunable worktrees pruned") || fd.Severity != "ok" {
		t.Errorf("finding = %+v", fd)
	}
}

func TestItemCheckoutsReadsBuildAndEvalDirsOnly(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"g/1", "g/1-eval", "g/0", "g/plan", "g/x-eval", ".claims/1", "g/2"} {
		if err := os.MkdirAll(filepath.Join(root, d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "g", "3"), 0o755); err != nil { // no .git
		t.Fatal(err)
	}
	var labels []string
	for _, c := range itemCheckouts(root) {
		labels = append(labels, c.Label)
	}
	if got, want := strings.Join(labels, " "), "g/1 g/1-eval g/2"; got != want {
		t.Errorf("checkouts = %q, want %q", got, want)
	}
}
