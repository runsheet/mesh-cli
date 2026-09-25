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

// INDEX.md carries, beside the documents, the project's live goals with
// a row per item; a finished goal is one line; another project's goal is
// not there.
func TestIndexListsLiveGoalsAndItems(t *testing.T) {
	f := seedLive(t)
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh")
	if err != nil {
		t.Fatal(err)
	}
	docsOnly := RenderIndex(cx)
	AddLive(context.Background(), gw, cx, orgID, projID, t.Logf)
	dir := t.TempDir()
	if _, err := WriteContext(dir, cx); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ContextDir, IndexFile))
	index := string(b)
	if !strings.HasPrefix(index, docsOnly) {
		t.Fatalf("the documents table comes first, unchanged:\n%s", index)
	}
	live := strings.TrimPrefix(index, docsOnly)
	want := "\n## Live goals and items\n\n" +
		"Read from the platform when this session started — the one place to read status from; no status table in git is current. Not committed.\n" +
		"\n### `docs` — Docs \\| all (paused)\n\n" +
		"| seq | title | status | branch | PR |\n| --- | --- | --- | --- | --- |\n" +
		"| 1 | Write the guide | merged | `mesh/docs-1` | https://github.com/acme/widgets/pull/7 |\n" +
		"| 2 | Link it | evaluating | `mesh/docs-2` | — |\n" +
		"\n### `ship-it` — Ship it (running)\n\n" +
		"| seq | title | status | branch | PR |\n| --- | --- | --- | --- | --- |\n" +
		"| 1 | Write the failing test | running | `mesh/ship-it-1` | — |\n" +
		"| 2 | Make it pass | pending | — | — |\n" +
		"\n### Finished goals\n\n" +
		"- `old-work` — Old work (done)\n"
	if live != want {
		t.Fatalf("live section:\n%s\nwant:\n%s", live, want)
	}
	if strings.Contains(index, "Buried") || strings.Contains(index, "not-ours") {
		t.Fatalf("a finished goal is not expanded; another project's goal is not listed:\n%s", index)
	}
	for _, r := range f.Reads {
		if r.Name == "workItemIndexs" && r.Input["goal_id"] == goal3 {
			t.Fatal("a finished goal's items are not read")
		}
	}
}

// When the platform's read fails, INDEX.md is the documents-only index
// it was before, and a warning is logged.
func TestIndexWithoutLiveStateIsDocumentsOnly(t *testing.T) {
	for _, fail := range []string{"goalIndexs", "workItemIndexs"} {
		f := seedLive(t)
		f.FailReads[fail] = "Cannot query field"
		gw := gateway.New(f.URL(), "rs_test")
		cx, err := FetchContext(context.Background(), gw, "acme", "mesh")
		if err != nil {
			t.Fatal(err)
		}
		docsOnly := RenderIndex(cx)
		var logged []string
		AddLive(context.Background(), gw, cx, orgID, projID, func(format string, args ...any) { logged = append(logged, format) })
		dir := t.TempDir()
		if _, err := WriteContext(dir, cx); err != nil {
			t.Fatal(err)
		}
		index, _ := os.ReadFile(filepath.Join(dir, ContextDir, IndexFile))
		if string(index) != docsOnly || strings.Contains(docsOnly, "Live goals") {
			t.Fatalf("%s failing: index:\n%s", fail, index)
		}
		if len(logged) != 1 || !strings.HasPrefix(logged[0], "warning:") {
			t.Fatalf("%s failing: logged %v", fail, logged)
		}
	}
	// a platform with no goals for the project: no section, no warning
	f := seed(t)
	delete(f.Goals, goalID)
	gw := gateway.New(f.URL(), "rs_test")
	cx, _ := FetchContext(context.Background(), gw, "acme", "mesh")
	docsOnly := RenderIndex(cx)
	AddLive(context.Background(), gw, cx, orgID, projID, func(string, ...any) { t.Fatal("no goals is not a warning") })
	if RenderIndex(cx) != docsOnly {
		t.Fatal("no goals: documents only")
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
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh")
	if err != nil {
		t.Fatal(err)
	}
	AddLive(context.Background(), gw, cx, orgID, projID, t.Logf)
	if _, err := WriteContext(dir, cx); err != nil {
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
