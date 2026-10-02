package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// seedGoal is seed's goal as the platform serves it.
func seedGoal(t *testing.T, gw *gateway.Client) *gateway.Goal {
	t.Helper()
	g, err := gw.Goal(context.Background(), goalID)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// widgets is the session's own repo in seed.
var widgets = []string{"acme/widgets"}

// Only the session's own repo's instructions go on disk: no ADR, PRD or
// TRD, whatever their size; INDEX.md says how to fetch them.
func TestContextSizeBudget(t *testing.T) {
	f := seed(t)
	f.Documents["acme/mesh"] = append(f.Documents["acme/mesh"], map[string]any{"slug": "trd", "title": "TRD", "kind": "trd", "repo": "acme/widgets", "path": "specs/trd.md"})
	f.Bodies["acme/mesh/trd"] = "# TRD"
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: widgets})
	if err != nil {
		t.Fatal(err)
	}
	if cx.Project != "acme/mesh" || len(cx.Index) != 5 || cx.PRDSlug != "prd" {
		t.Fatalf("context: project %s, %d in the index, prd %q", cx.Project, len(cx.Index), cx.PRDSlug)
	}
	dir := t.TempDir()
	names, _, err := WriteContext(dir, cx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "instructions-acme-widgets-agents.md" {
		t.Fatalf("written: %v", names)
	}
	for _, r := range f.Reads {
		if slug, _ := r.Input["slug"].(string); slug == "adr-001" || slug == "prd" || slug == "trd" {
			t.Fatalf("a design body was fetched: %v", r)
		}
	}
	index, _ := os.ReadFile(filepath.Join(dir, ContextDir, IndexFile))
	if !strings.Contains(string(index), "`mesh_document`") || !strings.Contains(string(index), "`mesh_search`") {
		t.Fatalf("index:\n%s", index)
	}
	mcp := MeshContextMCP("https://api.test/", "rs_x")
	srv := mcp["mesh-context"].(map[string]any)
	if srv["url"] != "https://api.test/mcp" || srv["headers"].(map[string]string)["Authorization"] != "Bearer rs_x" {
		t.Fatalf("mcp: %v", mcp)
	}
	if MeshContextMCP("", "rs_x") != nil || MeshContextMCP("https://api.test", "") != nil {
		t.Fatal("no gateway or no token: no server")
	}
}

// No document's summary, title or slug is listed in INDEX.md: the
// listing is fetched with mesh_search, not read from disk.
func TestRenderIndexSummaries(t *testing.T) {
	f := seed(t)
	f.Documents["acme/mesh"][0]["summary"] = "What the ADR decides."
	f.Documents["acme/mesh"] = append(f.Documents["acme/mesh"],
		map[string]any{"slug": "deploy", "title": "Deploy", "kind": "skill", "summary": "How to deploy.", "repo": "acme/widgets", "path": "skills/deploy.md"},
	)
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: widgets})
	if err != nil {
		t.Fatal(err)
	}
	index := RenderIndex(cx)
	for _, gone := range []string{"What the ADR decides.", "How to deploy.", "`adr-001`", "`deploy`", "Deploy", "| kind"} {
		if strings.Contains(index, gone) {
			t.Fatalf("INDEX.md carries %q:\n%s", gone, index)
		}
	}
	if !hasLine(index, "5 documents indexed, none listed here.") {
		t.Fatalf("no count line:\n%s", index)
	}
}

// INDEX.md is a screen: the project, what is on disk, the binding
// instructions left off it, how to find the rest, the ADRs and TRD as
// binding, one count line, then the session's own goal.
func TestRenderIndexTwoTables(t *testing.T) {
	cx := &Context{
		Project: "acme/mesh",
		Index: []gateway.Document{
			{Slug: "agents", Title: "AGENTS.md", Kind: "instructions", Repo: "acme/widgets", Path: "AGENTS.md"},
			{Slug: "claude", Title: "CLAUDE.md", Kind: "instructions", Repo: "acme/widgets", Path: "CLAUDE.md"},
			{Slug: "adr-002", Title: "ADR 002", Kind: "adr", Repo: "acme/widgets", Path: "docs/adr-002.md"},
			{Slug: "deploy", Title: "Deploy", Kind: "skill", Repo: "acme/widgets", Path: "skills/deploy.md"},
		},
		OnDisk:  map[string]string{"agents": "instructions-acme-widgets-agents.md"},
		Dropped: []gateway.Document{{Slug: "claude", Title: "CLAUDE.md", Kind: "instructions", Repo: "acme/widgets"}},
		Live: &LiveGoal{
			Goal:  gateway.Goal{Slug: "ship-it", Title: "Ship | it", Status: "running", Statement: "Make the thing.", Acceptance: "go test ./... passes"},
			Items: []gateway.WorkItem{{Seq: 1, Title: "Write the test", Status: "merged", RepoID: "r1", Branch: "mesh/ship-it-1"}, {Seq: 2, Title: "Make it pass", Status: "pending"}},
			Repos: map[string]string{"r1": "acme/widgets"},
		},
	}
	want := "# Project acme/mesh\n\n" +
		"## On disk here\n\n" +
		"- `instructions-acme-widgets-agents.md` — the instructions of acme/widgets, binding\n" +
		"- this session's goal, `ship-it`, below: its statement, acceptance and items\n" +
		"\n## Binding, not on disk — fetch first\n\n" +
		"- `claude` — the instructions of acme/widgets, binding, past the context cap: fetch it with `mesh_document` before you start\n" +
		"\n## Everything else: fetch it\n\n" +
		"Search before you read, and read a document only when the work touches it.\n\n" +
		"- `mesh_search` (project \"acme/mesh\", a query) → slug, title, kind, summary\n" +
		"- `mesh_document` (project \"acme/mesh\", a slug) → the body\n" +
		"- `mesh_documents` and `mesh_projects` → another project's documents\n" +
		"- `mesh_items` → live state: goals and items as the platform holds them now\n" +
		"\nThe ADRs and the TRD are binding: search them with `mesh_search` before any design decision.\n" +
		"\n4 documents indexed, none listed here.\n" +
		"\n## Goal `ship-it` — Ship \\| it (running)\n\n" +
		"Read from the platform when this session started; not committed.\n" +
		"\n### Statement\n\nMake the thing.\n" +
		"\n### Acceptance\n\ngo test ./... passes\n" +
		"\n### Items\n\n" +
		"| seq | title | status | repo | branch |\n| --- | --- | --- | --- | --- |\n" +
		"| 1 | Write the test | merged | acme/widgets | `mesh/ship-it-1` |\n" +
		"| 2 | Make it pass | pending | — | — |\n"
	if got := RenderIndex(cx); got != want {
		t.Fatalf("index:\n%s\nwant:\n%s", got, want)
	}
}

// INDEX.md is inside bytes_max, its goal section included, and
// IndexBytes is its length as written.
func TestIndexIsInsideTheBudget(t *testing.T) {
	f := seed(t)
	f.Bodies["acme/mesh/agents"] = strings.Repeat("a", 2000)
	gw := gateway.New(f.URL(), "rs_test")
	goal := seedGoal(t, gw)
	bare, _ := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Goal: goal})
	bytesMax := bare.IndexBytes + 2000 + 200 // room for the file and its line
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: bytesMax}, Repos: widgets, Goal: goal})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	_, w, err := WriteContext(dir, cx)
	if err != nil {
		t.Fatal(err)
	}
	index, _ := os.ReadFile(filepath.Join(dir, ContextDir, IndexFile))
	if cx.IndexBytes != len(index) || w.Bytes > bytesMax || len(cx.OnDisk) != 1 {
		t.Fatalf("IndexBytes %d, INDEX.md %d, written %d of %d, on disk %v", cx.IndexBytes, len(index), w.Bytes, bytesMax, cx.OnDisk)
	}
	// ten bytes less: the instructions no longer fit and go to the index
	cx, _ = FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: w.Bytes - 10}, Repos: widgets, Goal: goal})
	if len(cx.OnDisk) != 0 || len(cx.Dropped) != 1 || cx.IndexBytes > w.Bytes-10 {
		t.Fatalf("on disk %v, dropped %v, index %d", cx.OnDisk, cx.Dropped, cx.IndexBytes)
	}
}

// An index alone past bytes_max puts no document on disk and says so
// once; the own repo's instructions are named to fetch.
func TestIndexAlonePastTheBudget(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: 100}, Repos: widgets, Goal: seedGoal(t, gw), Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	if len(cx.Docs) != 0 || len(cx.OnDisk) != 0 {
		t.Fatalf("on disk: %v", cx.OnDisk)
	}
	if got := strings.Join(slugsOf(cx.Dropped), ","); got != "agents" {
		t.Fatalf("dropped: %s", got)
	}
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "warning: INDEX.md alone is ") || !strings.Contains(logged[0], "past bytes_max 100") {
		t.Fatalf("logged %q", logged)
	}
	if !strings.Contains(RenderIndex(cx), "- `agents` — the instructions of acme/widgets, binding, past the context cap") {
		t.Fatalf("index:\n%s", RenderIndex(cx))
	}
}

func slugsOf(docs []gateway.Document) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.Slug
	}
	return out
}

// seedInstructions adds instructions documents of n bytes each, by repo.
func seedInstructions(f *gatewaytest.Fake, n int, docs ...[2]string) {
	for _, d := range docs {
		slug, repo := d[0], d[1]
		f.Documents["acme/mesh"] = append(f.Documents["acme/mesh"], map[string]any{"slug": slug, "title": slug, "kind": "instructions", "repo": repo, "path": slug + ".md"})
		f.Bodies["acme/mesh/"+slug] = strings.Repeat("i", n)
	}
}

// Two own instructions documents and a cap that holds one: the first is
// on disk, the second left whole to INDEX.md as binding, never cut.
func TestContextBudgetHoldsForBindingDocuments(t *testing.T) {
	f := seed(t)
	f.Bodies["acme/mesh/agents"] = strings.Repeat("a", 3000)
	seedInstructions(f, 3000, [2]string{"claude", "acme/widgets"})
	gw := gateway.New(f.URL(), "rs_test")
	bare, _ := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{})
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: bare.IndexBytes + 4500}, Repos: widgets})
	if err != nil {
		t.Fatal(err)
	}
	if cx.OnDisk["agents"] == "" || len(cx.Docs) != 1 || strings.Join(slugsOf(cx.Dropped), ",") != "claude" {
		t.Fatalf("on disk %v, dropped %v", cx.OnDisk, slugsOf(cx.Dropped))
	}
	if cx.Docs["instructions-acme-widgets-agents.md"] != strings.Repeat("a", 3000)+"\n" { // the fake ends a body with a newline
		t.Fatalf("a placed document is whole: %v", cx.OnDisk)
	}
}

// Another repo's instructions never go on disk, however much room is
// left; nor does any ADR.
func TestContextBudgetOwnRepoFirst(t *testing.T) {
	f := seed(t)
	seedInstructions(f, 100, [2]string{"other-agents", "acme/other"})
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: []string{"ACME/Widgets"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cx.OnDisk) != 1 || cx.OnDisk["agents"] != "instructions-acme-widgets-agents.md" || len(cx.Dropped) != 0 {
		t.Fatalf("on disk %v, dropped %v", cx.OnDisk, slugsOf(cx.Dropped))
	}
	// no repos (mesh goal, channel mode): nothing on disk but the index
	cx, _ = FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{})
	if len(cx.OnDisk) != 0 {
		t.Fatalf("no repos: on disk %v", cx.OnDisk)
	}
}

// Own instructions alone past the cap still leave a usable context: the
// ones that fit, in the index's order, and the rest named to fetch.
func TestContextBudgetBindingAloneExceeds(t *testing.T) {
	f := seed(t)
	f.Bodies["acme/mesh/agents"] = strings.Repeat("a", 30*1024)
	seedInstructions(f, 50*1024, [2]string{"claude", "acme/widgets"})
	seedInstructions(f, 15*1024, [2]string{"small", "acme/widgets"})
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: widgets, Goal: seedGoal(t, gw)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(slugsOf(cx.Dropped), ",") != "claude" || len(cx.OnDisk) != 2 {
		t.Fatalf("on disk %v, dropped %v", cx.OnDisk, slugsOf(cx.Dropped))
	}
	_, w, err := WriteContext(t.TempDir(), cx)
	if err != nil || w.Bytes > DefaultContextBytesMax {
		t.Fatalf("written %+v: %v", w, err)
	}
	if !strings.HasPrefix(w.TargetWarning(), "warning: context ") || !strings.HasSuffix(w.TargetWarning(), "instructions-acme-widgets-small.md took it past") {
		t.Fatalf("target warning %q", w.TargetWarning())
	}
}

// The goal's acceptance, end to end: a project of 1,000 documents —
// ADRs, PRD, TRD, skills, two repos' instructions of 30KB — and 12 live
// goals of 20 items each. A build in one repo writes under 40KB: its
// repo's instructions and its own goal, no document row, no other goal.
func TestContextHoldsOnlyInstructionsAndOwnGoal(t *testing.T) {
	f := seed(t)
	docs := []map[string]any{
		{"slug": "agents", "title": "AGENTS.md", "kind": "instructions", "repo": "acme/widgets", "path": "AGENTS.md", "summary": "How to work in widgets."},
		{"slug": "other-agents", "title": "AGENTS.md", "kind": "instructions", "repo": "acme/other", "path": "AGENTS.md", "summary": "How to work in other."},
		{"slug": "prd", "title": "PRD", "kind": "prd", "repo": "acme/specs", "path": "specs/prd.md", "summary": "What."},
		{"slug": "trd", "title": "TRD", "kind": "trd", "repo": "acme/specs", "path": "specs/trd.md", "summary": "How."},
	}
	f.Bodies["acme/mesh/agents"] = strings.Repeat("w", 30*1024)
	f.Bodies["acme/mesh/other-agents"] = strings.Repeat("o", 30*1024)
	f.Bodies["acme/mesh/prd"] = strings.Repeat("p", 60*1024)
	f.Bodies["acme/mesh/trd"] = strings.Repeat("t", 60*1024)
	for i := 0; len(docs) < 1000; i++ {
		kind := "adr"
		if i%2 == 1 {
			kind = "skill"
		}
		slug := fmt.Sprintf("%s-%03d-a-decision-of-some-length", kind, i)
		docs = append(docs, map[string]any{"slug": slug, "title": fmt.Sprintf("Decision %d", i), "kind": kind, "repo": "acme/specs", "path": "specs/" + slug + ".md", "summary": "A one-line summary of what it decides."})
		f.Bodies["acme/mesh/"+slug] = strings.Repeat("d", 12*1024)
	}
	f.Documents["acme/mesh"] = docs
	var others []string
	for g := 0; g < 12; g++ {
		id := fmt.Sprintf("dddddddd-%04d-4000-8000-dddddddddddd", g)
		title := fmt.Sprintf("Live goal number %d of the estate", g)
		if g == 0 {
			id, title = goalID, "Ship it"
			delete(f.WorkItems, item1)
			delete(f.WorkItems, item2)
		} else {
			others = append(others, title)
		}
		f.Goals[id] = map[string]any{"id": id, "orgId": orgID, "projectId": projID, "slug": fmt.Sprintf("goal-%d", g), "title": title,
			"statement": strings.Repeat("Why this goal. ", 40), "acceptance": strings.Repeat("What proves it. ", 20), "status": "running"}
		for s := 1; s <= 20; s++ {
			iid := fmt.Sprintf("eeeeeeee-%04d-4000-8000-%012d", g, s)
			f.WorkItems[iid] = map[string]any{"id": iid, "goalId": id, "orgId": orgID, "seq": s, "title": fmt.Sprintf("Item %d of goal %d does one thing well", s, g),
				"repoId": repoID, "status": "merged", "branch": fmt.Sprintf("mesh/goal-%d-%d", g, s), "dependsOn": []string{}}
		}
	}
	gw := gateway.New(f.URL(), "rs_test")
	var logged []string
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: widgets, Goal: seedGoal(t, gw), Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	names, w, err := WriteContext(dir, cx)
	if err != nil {
		t.Fatal(err)
	}
	cdir := filepath.Join(dir, ContextDir)
	entries, _ := os.ReadDir(cdir)
	onDisk := 0
	for _, e := range entries {
		st, _ := os.Stat(filepath.Join(cdir, e.Name()))
		onDisk += int(st.Size())
		for _, p := range []string{"adr-*.md", "prd-*.md", "trd-*.md"} {
			if ok, _ := filepath.Match(p, e.Name()); ok {
				t.Fatalf("%s is on disk", e.Name())
			}
		}
	}
	t.Logf("1,000 documents, 12 live goals: %s; files %v", w.Line(), names)
	if w.Bytes >= 40*1024 || w.Bytes != onDisk || w.TargetWarning() != "" || len(logged) != 0 {
		t.Fatalf("written %+v, on disk %d, logged %q", w, onDisk, logged)
	}
	if strings.Join(names, ",") != "instructions-acme-widgets-agents.md" || w.ToFetch != 999 {
		t.Fatalf("files %v, %d to fetch", names, w.ToFetch)
	}
	b, _ := os.ReadFile(filepath.Join(cdir, IndexFile))
	index := string(b)
	for _, l := range strings.Split(index, "\n") {
		if strings.HasPrefix(l, "| kind") {
			t.Fatalf("INDEX.md has a document table:\n%s", index)
		}
	}
	for _, title := range others {
		if strings.Contains(index, title) {
			t.Fatalf("INDEX.md names another goal, %q", title)
		}
	}
	if !hasLine(index, "| 20 | Item 20 of goal 0 does one thing well | merged | acme/widgets | `mesh/goal-0-20` |") ||
		!hasLine(index, "1000 documents indexed, none listed here.") {
		t.Fatalf("INDEX.md lacks its own goal's items or the count:\n%s", index)
	}
}

// Instructions of 100KB stay off disk; INDEX.md names them as binding,
// to fetch, and the bytes stay under the 80KB cap.
func TestContextNeverPastTheCap(t *testing.T) {
	f := seed(t)
	f.Bodies["acme/mesh/agents"] = strings.Repeat("a", 100*1024)
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: widgets, Goal: seedGoal(t, gw)})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	names, w, err := WriteContext(dir, cx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("100KB instructions: %s", w.Line())
	if len(names) != 0 || w.Bytes >= 80*1024 || strings.Join(slugsOf(cx.Dropped), ",") != "agents" {
		t.Fatalf("files %v, %+v, dropped %v", names, w, slugsOf(cx.Dropped))
	}
	index, _ := os.ReadFile(filepath.Join(dir, ContextDir, IndexFile))
	const line = "- `agents` — the instructions of acme/widgets, binding, past the context cap: fetch it with `mesh_document` before you start"
	if !hasLine(string(index), line) {
		t.Fatalf("INDEX.md does not name the instructions to fetch:\n%s", index)
	}
}

// WriteContext's ContextWritten is what is on disk: the bytes of every
// document file and INDEX.md as read back, the document count, and the
// index rows with no file (adr-016).
func TestContextWritten(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: []string{"acme/widgets"}, Goal: seedGoal(t, gw)})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	names, w, err := WriteContext(dir, cx)
	if err != nil {
		t.Fatal(err)
	}
	onDisk := 0
	for _, n := range append(names, IndexFile) {
		st, err := os.Stat(filepath.Join(dir, ContextDir, n))
		if err != nil {
			t.Fatal(err)
		}
		onDisk += int(st.Size())
	}
	entries, _ := os.ReadDir(filepath.Join(dir, ContextDir))
	if len(entries) != len(names)+1 {
		t.Fatalf("%d files in the context dir, %d names and INDEX.md", len(entries), len(names))
	}
	fetch := 0
	for _, d := range cx.Index {
		if _, ok := cx.OnDisk[d.Slug]; !ok {
			fetch++
		}
	}
	if w.Bytes != onDisk || w.Files != len(names) || w.ToFetch != fetch || fetch == 0 {
		t.Fatalf("written %+v; on disk %d bytes in %d files, %d to fetch", w, onDisk, len(names), fetch)
	}
	want := fmt.Sprintf("context %d bytes in %d files, %d documents to fetch", onDisk, len(names), fetch)
	if w.Line() != want {
		t.Fatalf("line %q, want %q", w.Line(), want)
	}
	if _, w, _ := WriteContext(t.TempDir(), &Context{}); w != (ContextWritten{}) {
		t.Fatalf("no context: %+v", w)
	}
}
