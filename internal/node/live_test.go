package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const (
	goal2  = "dddddddd-2222-4222-8222-dddddddddddd"
	goal3  = "dddddddd-3333-4333-8333-dddddddddddd"
	other  = "dddddddd-4444-4444-8444-dddddddddddd"
	item21 = "eeeeeeee-2222-4222-8222-eeeeeeeeeeee"
	item22 = "eeeeeeee-3333-4333-8333-eeeeeeeeeeee"
	item31 = "eeeeeeee-4444-4444-8444-eeeeeeeeeeee"
)

// seedLive adds to seed's goal (running, items ready and pending) a
// second live goal with a merged and a running item, a finished goal
// with an item, and a goal of another project.
func seedLive(t *testing.T) *gatewaytest.Fake {
	t.Helper()
	f := seed(t)
	f.WorkItems[item1]["status"], f.WorkItems[item1]["branch"] = "running", "mesh/ship-it-1"
	f.Goals[goal2] = map[string]any{"id": goal2, "orgId": orgID, "projectId": projID, "slug": "docs", "title": "Docs | all", "status": "paused"}
	f.WorkItems[item21] = map[string]any{"id": item21, "goalId": goal2, "orgId": orgID, "seq": 1, "title": "Write the guide", "status": "merged",
		"branch": "mesh/docs-1", "prUrl": "https://github.com/acme/widgets/pull/7"}
	f.WorkItems[item22] = map[string]any{"id": item22, "goalId": goal2, "orgId": orgID, "seq": 2, "title": "Link it", "status": "evaluating", "branch": "mesh/docs-2"}
	f.Goals[goal3] = map[string]any{"id": goal3, "orgId": orgID, "projectId": projID, "slug": "old-work", "title": "Old work", "status": "done"}
	f.WorkItems[item31] = map[string]any{"id": item31, "goalId": goal3, "orgId": orgID, "seq": 1, "title": "Buried", "status": "merged", "branch": "mesh/old-work-1"}
	f.Goals[other] = map[string]any{"id": other, "orgId": orgID, "projectId": "elsewhere", "slug": "not-ours", "title": "Not ours", "status": "running"}
	return f
}

// INDEX.md carries the session's own goal — statement, acceptance and
// a row per item with its repo and branch — and no other goal, live,
// finished or of another project.
func TestIndexListsLiveGoalsAndItems(t *testing.T) {
	f := seedLive(t)
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: widgets, Goal: seedGoal(t, gw), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, _, err := WriteContext(dir, cx); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ContextDir, IndexFile))
	index := string(b)
	_, goal, ok := strings.Cut(index, "\n## Goal ")
	if !ok {
		t.Fatalf("no goal section:\n%s", index)
	}
	want := "`ship-it` — Ship it (running)\n\n" +
		"Read from the platform when this session started; not committed.\n" +
		"\n### Statement\n\nMake the thing.\n" +
		"\n### Acceptance\n\ngo test ./... passes\n" +
		"\n### Items\n\n" +
		"| seq | title | status | repo | branch |\n| --- | --- | --- | --- | --- |\n" +
		"| 1 | Write the failing test | running | acme/widgets | `mesh/ship-it-1` |\n" +
		"| 2 | Make it pass | pending | acme/widgets | — |\n"
	if goal != want {
		t.Fatalf("goal section:\n%s\nwant:\n%s", goal, want)
	}
	for _, other := range []string{"docs", "Write the guide", "old-work", "Buried", "not-ours"} {
		if strings.Contains(index, other) {
			t.Fatalf("INDEX.md names %q, of another goal:\n%s", other, index)
		}
	}
	for _, r := range f.Reads {
		if r.Name == "goalIndexs" || (r.Name == "workItemIndexs" && r.Input["goal_id"] != nil && r.Input["goal_id"] != goalID) {
			t.Fatalf("another goal was read: %v", r)
		}
	}
}

// When the items cannot be read, the goal's statement and acceptance
// are still there, the items are left to mesh_items, and a warning is
// logged; a session with no goal (research) has no goal section.
func TestIndexWithoutLiveStateIsDocumentsOnly(t *testing.T) {
	f := seedLive(t)
	f.FailReads["workItemIndexs"] = "Cannot query field"
	gw := gateway.New(f.URL(), "rs_test")
	var logged []string
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Goal: seedGoal(t, gw), Logf: func(format string, args ...any) { logged = append(logged, format) }})
	if err != nil {
		t.Fatal(err)
	}
	index := RenderIndex(cx)
	if !strings.Contains(index, "### Acceptance\n\ngo test ./... passes\n") || !strings.HasSuffix(index, "### Items\n\nUnread here: ask `mesh_items`.\n") {
		t.Fatalf("index:\n%s", index)
	}
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "warning:") {
		t.Fatalf("logged %v", logged)
	}
	cx, _ = FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Logf: func(string, ...any) { t.Fatal("no goal is not a warning") }})
	if cx.Live != nil || strings.Contains(RenderIndex(cx), "## Goal") {
		t.Fatal("no goal: no goal section")
	}
}

// INDEX.md never reaches the item's commit: the whole of .mesh is
// excluded from git, even under `git add -A`.
func TestIndexIsNeverCommitted(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	f := seedLive(t)
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: widgets, Goal: seedGoal(t, gw)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteContext(dir, cx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ContextDir, IndexFile)); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "work.go"), []byte("package work\n"), 0o644)
	git("add", "-A")
	if staged := git("diff", "--cached", "--name-only"); staged != "work.go" {
		t.Fatalf("staged: %q", staged)
	}
	git("commit", "-q", "-m", "the item's work")
	if committed := git("show", "--name-only", "--format=", "HEAD"); committed != "work.go" {
		t.Fatalf("committed: %q", committed)
	}
}
