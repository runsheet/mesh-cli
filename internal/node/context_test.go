package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	mcp := MeshContextHTTP("https://api.test/", "rs_x")
	srv := mcp["mesh-context"].(map[string]any)
	if srv["url"] != "https://api.test/mcp" || srv["headers"].(map[string]string)["Authorization"] != "Bearer rs_x" {
		t.Fatalf("mcp: %v", mcp)
	}
	if MeshContextHTTP("", "rs_x") != nil || MeshContextHTTP("https://api.test", "") != nil {
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
		OnDisk:  map[string]gateway.Document{"instructions-acme-widgets-agents.md": {Slug: "agents", Kind: "instructions", Repo: "acme/widgets", Path: "AGENTS.md"}},
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
	// ten bytes less: the instructions are binding and stay, the goal's
	// item rows give way to mesh_items
	cx, _ = FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: w.Bytes - 10}, Repos: widgets, Goal: goal})
	if len(cx.OnDisk) != 1 || len(cx.Dropped) != 0 || cx.IndexBytes+2001 > w.Bytes-10 || !strings.Contains(RenderIndex(cx), "ask `mesh_items`") {
		t.Fatalf("on disk %v, dropped %v, index %d", cx.OnDisk, cx.Dropped, cx.IndexBytes)
	}
	// a cap the instructions alone pass: they go to the index
	cx, _ = FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: 1900}, Repos: widgets, Goal: goal})
	if len(cx.OnDisk) != 0 || len(cx.Dropped) != 1 || cx.IndexBytes > 1900 {
		t.Fatalf("on disk %v, dropped %v, index %d", cx.OnDisk, cx.Dropped, cx.IndexBytes)
	}
}

// writtenBytes is every file under the context directory as read back.
func writtenBytes(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, ContextDir))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, ContextDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		n += len(b)
	}
	return n
}

// An own goal whose section alone is past the cap: the goal section is
// shortened to fit — its items to mesh_items, then its statement and
// acceptance — and the own repo's instructions, binding, stay on disk.
// A cap smaller than the index's own lines says so once, puts no
// document on disk and names the instructions to fetch. What is
// written, read back, is inside the cap.
func TestIndexAlonePastTheBudget(t *testing.T) {
	f := seed(t)
	f.Goals[goalID]["statement"] = strings.Repeat("Why this goal matters. ", 3000) // ~69KB
	f.Goals[goalID]["acceptance"] = strings.Repeat("What proves it. ", 1000)       // ~16KB
	for s := 3; s <= 400; s++ {
		iid := fmt.Sprintf("eeeeeeee-0000-4000-8000-%012d", s)
		f.WorkItems[iid] = map[string]any{"id": iid, "goalId": goalID, "orgId": orgID, "seq": s, "title": strings.Repeat("A long item title ", 5),
			"repoId": repoID, "status": "pending", "dependsOn": []string{}}
	}
	gw := gateway.New(f.URL(), "rs_test")
	for _, c := range []struct {
		name     string
		bytesMax int
		want     string
		dropped  string
	}{
		{"default cap, statement and acceptance past it", 0, "Its statement, acceptance and items are past the context cap: ask `mesh_items`.", ""},
		{"a cap smaller than the index's own lines", 300, "… INDEX.md cut at the context cap (300 bytes)", "agents"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged []string
			logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
			cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: c.bytesMax}, Repos: widgets, Goal: seedGoal(t, gw), Logf: logf})
			if err != nil {
				t.Fatal(err)
			}
			if len(cx.Docs)+len(cx.Dropped) != 1 || strings.Join(slugsOf(cx.Dropped), ",") != c.dropped {
				t.Fatalf("on disk %v, dropped %v", cx.OnDisk, slugsOf(cx.Dropped))
			}
			if (c.dropped == "") != (len(logged) == 0) || len(logged) > 1 || len(logged) == 1 && !strings.HasPrefix(logged[0], "warning: INDEX.md alone is ") {
				t.Fatalf("logged %q", logged)
			}
			dir := t.TempDir()
			_, w, err := WriteContext(dir, cx)
			if err != nil {
				t.Fatal(err)
			}
			cap := cx.bytesMax()
			if got := writtenBytes(t, dir); got != w.Bytes || got > cap || c.dropped != "" && cx.IndexBytes != got {
				t.Fatalf("written %d, reported %d, IndexBytes %d, cap %d", got, w.Bytes, cx.IndexBytes, cap)
			}
			index, _ := os.ReadFile(filepath.Join(dir, ContextDir, IndexFile))
			if !strings.Contains(string(index), c.want) {
				t.Fatalf("INDEX.md lacks %q:\n%s", c.want, index)
			}
			if c.bytesMax == 0 && !strings.Contains(string(index), "- `instructions-acme-widgets-agents.md` — the instructions of acme/widgets, binding") {
				t.Fatalf("INDEX.md does not name the instructions:\n%s", index)
			}
		})
	}
}

// The own goal's item table alone past the 80KB cap: the bytes on disk
// after WriteContext are inside it, the table is cut at a row boundary
// — every row written whole — and INDEX.md sends the rest to
// mesh_items; the statement, acceptance and instructions stay.
func TestOwnGoalItemTablePastTheCapIsCutAtARow(t *testing.T) {
	f := seed(t)
	for s := 3; s <= 800; s++ {
		iid := fmt.Sprintf("eeeeeeee-0000-4000-8000-%012d", s)
		f.WorkItems[iid] = map[string]any{"id": iid, "goalId": goalID, "orgId": orgID, "seq": s, "title": strings.Repeat("A long item title ", 5),
			"repoId": repoID, "status": "pending", "branch": fmt.Sprintf("mesh/ship-it-%d", s), "dependsOn": []string{}}
	}
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: widgets, Goal: seedGoal(t, gw)})
	if err != nil {
		t.Fatal(err)
	}
	var table strings.Builder
	renderOwnGoal(&table, cx.Live, func(s string) string { return s }, indexFull, -1)
	if table.Len() <= DefaultContextBytesMax {
		t.Fatalf("the goal section is %d bytes, not past the cap", table.Len())
	}
	dir := t.TempDir()
	_, w, err := WriteContext(dir, cx)
	if err != nil {
		t.Fatal(err)
	}
	if got := writtenBytes(t, dir); got != w.Bytes || got > DefaultContextBytesMax {
		t.Fatalf("written %d, reported %d, cap %d", got, w.Bytes, DefaultContextBytesMax)
	}
	t.Logf("800 items: %s", w.Line())
	b, _ := os.ReadFile(filepath.Join(dir, ContextDir, IndexFile))
	index := string(b)
	rows, last := 0, 0
	for _, l := range strings.Split(index, "\n") {
		if !strings.HasPrefix(l, "| ") || strings.HasPrefix(l, "| seq") || strings.HasPrefix(l, "| ---") {
			continue
		}
		if !strings.HasSuffix(l, "` |") && !strings.HasSuffix(l, "— |") {
			t.Fatalf("a row cut short: %q", l)
		}
		rows++
		_, _ = fmt.Sscanf(l, "| %d |", &last)
	}
	want := fmt.Sprintf("… %d more items past the context cap (seq %d on): ask `mesh_items` for the rest.", 800-rows, last+1)
	if rows == 0 || rows == 800 || !hasLine(index, want) || !strings.Contains(index, "Make the thing.") || len(cx.Docs) != 1 {
		t.Fatalf("%d rows, want line %q, docs %d:\n%s", rows, want, len(cx.Docs), index[len(index)-600:])
	}
}

// A dropped document's line grows INDEX.md after the documents before
// it were placed: the cap is checked again on what is written, and a
// placed document leaves the disk for the index when it must.
func TestDroppedLinesNeverPushPastTheCap(t *testing.T) {
	cx := &Context{Project: "acme/mesh", BytesMax: 4000, Docs: map[string]string{}, OnDisk: map[string]gateway.Document{}}
	for i := 0; i < 3; i++ {
		d := gateway.Document{Slug: fmt.Sprintf("agents-%d", i), Kind: "instructions", Repo: "acme/widgets"}
		cx.Index = append(cx.Index, d)
		name := contextFileName(d, nil)
		cx.Docs[name] = strings.Repeat("a", 900)
		cx.OnDisk[name] = d
	}
	bare := len(RenderIndex(cx))
	// documents and index exactly at the cap, then three dropped lines
	cx.BytesMax = bare + 3*900
	for i := 0; i < 3; i++ {
		d := gateway.Document{Slug: fmt.Sprintf("dropped-%02d-%s", i, strings.Repeat("x", 40)), Kind: "instructions", Repo: "acme/widgets"}
		cx.Index = append(cx.Index, d)
		cx.Dropped = append(cx.Dropped, d)
	}
	dir := t.TempDir()
	names, w, err := WriteContext(dir, cx)
	if err != nil {
		t.Fatal(err)
	}
	if got := writtenBytes(t, dir); got != w.Bytes || got > cx.BytesMax {
		t.Fatalf("written %d, reported %d, cap %d", got, w.Bytes, cx.BytesMax)
	}
	if len(names) == 3 || len(cx.Dropped) != 3+3-len(names) {
		t.Fatalf("files %v, dropped %v", names, slugsOf(cx.Dropped))
	}
	index, _ := os.ReadFile(filepath.Join(dir, ContextDir, IndexFile))
	if !strings.Contains(string(index), "- `agents-2` — the instructions of acme/widgets, binding, past the context cap") {
		t.Fatalf("INDEX.md does not name the document it took off disk:\n%s", index)
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
	if cx.OnDisk["instructions-acme-widgets-agents.md"].Slug != "agents" || len(cx.Docs) != 1 || strings.Join(slugsOf(cx.Dropped), ",") != "claude" {
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
	if len(cx.OnDisk) != 1 || cx.OnDisk["instructions-acme-widgets-agents.md"].Slug != "agents" || len(cx.Dropped) != 0 {
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
// ADRs, PRD, TRD, skills, three repos' AGENTS.md all of slug `agents` —
// and 12 live goals of 20 items each. A session whose repos are two of
// the three writes under 40KB: exactly those two repos' instructions,
// each its own repo's body under its own repo-named file, its own goal
// and INDEX.md; no document row, no other goal, and the tools named.
func TestContextHoldsOnlyInstructionsAndOwnGoal(t *testing.T) {
	f := seed(t)
	docs := []map[string]any{
		{"slug": "agents", "title": "AGENTS.md", "kind": "instructions", "repo": "acme/widgets", "path": "AGENTS.md", "summary": "How to work in widgets."},
		{"slug": "agents", "title": "AGENTS.md", "kind": "instructions", "repo": "acme/gears", "path": "AGENTS.md", "summary": "How to work in gears."},
		{"slug": "agents", "title": "AGENTS.md", "kind": "instructions", "repo": "acme/mesh-cli", "path": "AGENTS.md", "summary": "How to work in mesh-cli."},
		{"slug": "prd", "title": "PRD", "kind": "prd", "repo": "acme/specs", "path": "specs/prd.md", "summary": "What."},
		{"slug": "trd", "title": "TRD", "kind": "trd", "repo": "acme/specs", "path": "specs/trd.md", "summary": "How."},
	}
	// the gateway's by-slug read answers one repo's body for all three
	f.Bodies["acme/mesh/agents"] = strings.Repeat("m", 15*1024)
	checkouts := map[string]string{}
	bodies := map[string]string{}
	for repo, c := range map[string]string{"acme/widgets": "w", "acme/gears": "g"} {
		dir := t.TempDir()
		bodies[repo] = "# " + repo + "\n" + strings.Repeat(c, 15*1024)
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(bodies[repo]), 0o644); err != nil {
			t.Fatal(err)
		}
		checkouts[repo] = dir
	}
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
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: []string{"acme/widgets", "acme/gears"}, Checkouts: checkouts, Goal: seedGoal(t, gw), Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }})
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
	if strings.Join(names, ",") != "instructions-acme-gears-agents.md,instructions-acme-widgets-agents.md" || w.ToFetch != 998 || len(entries) != 3 {
		t.Fatalf("files %v, %d to fetch, %d entries", names, w.ToFetch, len(entries))
	}
	for repo, body := range bodies {
		name := "instructions-" + Slugify(repo) + "-agents.md"
		got, _ := os.ReadFile(filepath.Join(cdir, name))
		if string(got) != body {
			t.Fatalf("%s is not %s's own AGENTS.md: %.40q", name, repo, got)
		}
	}
	b, _ := os.ReadFile(filepath.Join(cdir, IndexFile))
	index := string(b)
	// the only table is the own goal's items: no row for a document
	for _, l := range strings.Split(index, "\n") {
		if strings.HasPrefix(l, "|") && !strings.HasPrefix(l, "| seq |") && !strings.HasPrefix(l, "| --- |") && !strings.Contains(l, "| `mesh/goal-0-") {
			t.Fatalf("INDEX.md has a row that is not its own goal's item: %q", l)
		}
	}
	for _, d := range docs[3:] {
		if strings.Contains(index, "`"+d["slug"].(string)+"`") || strings.Contains(index, d["summary"].(string)) {
			t.Fatalf("INDEX.md names %s, not on disk", d["slug"])
		}
	}
	for g := 1; g < 12; g++ {
		if strings.Contains(index, fmt.Sprintf("`goal-%d`", g)) || strings.Contains(index, others[g-1]) {
			t.Fatalf("INDEX.md names another goal, goal-%d", g)
		}
	}
	for _, repo := range []string{"acme/widgets", "acme/gears"} {
		if !hasLine(index, "- `instructions-"+Slugify(repo)+"-agents.md` — the instructions of "+repo+", binding") {
			t.Fatalf("INDEX.md does not list %s's file:\n%s", repo, index)
		}
	}
	for _, tool := range []string{"`mesh_search`", "`mesh_document`", "`mesh_items`", "`mesh_documents`", "`mesh_projects`"} {
		if !strings.Contains(index, tool) {
			t.Fatalf("INDEX.md does not name %s", tool)
		}
	}
	if strings.Contains(index, "mesh-cli") || strings.Count(index, "\n") > 60 {
		t.Fatalf("INDEX.md names a repo not the session's, or is past a screen (%d lines)", strings.Count(index, "\n"))
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
		if _, ok := cx.OnDisk[contextFileName(d, nil)]; !ok || d.Kind != "instructions" {
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

// A reused checkout keeps the last session's .mesh/context. When the
// context read fails the session writes an empty context (RunItem's
// cx = &Context{}), and what an earlier node left there — ADR bodies,
// an INDEX.md past the cap — must not ride along: the directory is
// cleared and the bytes reported are the bytes on disk, none.
func TestContextReadFailureClearsAReusedCheckout(t *testing.T) {
	// nothing listens here: the documents read fails, once its retries'
	// window — shortened so the test runs fast — is spent
	orig := gateway.ReadRetryFor
	gateway.ReadRetryFor = 50 * time.Millisecond
	t.Cleanup(func() { gateway.ReadRetryFor = orig })
	gw := gateway.New("http://127.0.0.1:1", "rs_test")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	cdir := filepath.Join(dir, ContextDir)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := map[string]int{
		"adr-adr-001-item-scoped-github-token.md":  20 * 1024,
		"adr-adr-002-signed-tag-releases.md":       14 * 1024,
		"adr-adr-009-sessions-outlive-the-node.md": 17 * 1024,
		IndexFile: 150 * 1024,
	}
	for n, size := range stale {
		if err := os.WriteFile(filepath.Join(cdir, n), []byte(strings.Repeat("x", size)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: widgets})
	if err == nil {
		t.Fatalf("read from a gateway that is not there succeeded: %+v", cx)
	}
	cx = &Context{}
	names, w, err := WriteContext(dir, cx)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 || w != (ContextWritten{}) {
		t.Fatalf("empty context wrote %v, %+v", names, w)
	}
	onDisk := 0
	_ = filepath.WalkDir(filepath.Join(dir, ".mesh"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			st, _ := d.Info()
			onDisk += int(st.Size())
			t.Errorf("left on disk: %s (%d bytes)", p, st.Size())
		}
		return nil
	})
	if onDisk != w.Bytes {
		t.Fatalf("%d bytes on disk, %d reported", onDisk, w.Bytes)
	}
	// and a nil context clears it the same
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(cdir, IndexFile), []byte("stale"), 0o644)
	if _, _, err := WriteContext(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cdir); !os.IsNotExist(err) {
		t.Fatalf("nil context left %s: %v", cdir, err)
	}
}

// A shared instructions slug with no checkout of its repo is never read
// by slug — the gateway would answer another repo's body — and INDEX.md
// names it by repo and path, to read before starting.
func TestSharedInstructionsSlugWithoutACheckoutIsNamedByRepo(t *testing.T) {
	f := seed(t)
	f.Documents["acme/mesh"] = append(f.Documents["acme/mesh"], map[string]any{"slug": "agents", "title": "AGENTS.md", "kind": "instructions", "repo": "acme/other", "path": "AGENTS.md"})
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: widgets})
	if err != nil {
		t.Fatal(err)
	}
	// the gateway holds a body for `agents`: none on disk is none read
	if f.Bodies["acme/mesh/agents"] == "" || len(cx.Docs) != 0 || strings.Join(slugsOf(cx.Dropped), ",") != "agents" {
		t.Fatalf("on disk %v, dropped %v", cx.OnDisk, slugsOf(cx.Dropped))
	}
	const line = "- `AGENTS.md` of acme/widgets — its instructions, binding, not on disk: read `AGENTS.md` in acme/widgets's checkout before you start (slug `agents` is shared by several repos' instructions)"
	if index := RenderIndex(cx); !hasLine(index, line) {
		t.Fatalf("INDEX.md:\n%s", index)
	}
}

// Two instructions documents of one repo with one slug at different
// paths are two files: each its own body under its own name, each its
// own INDEX.md line. Named by slug they would overwrite each other.
func TestInstructionsOfOneRepoAreKeyedByPath(t *testing.T) {
	f := seed(t)
	f.Documents["acme/mesh"] = append(f.Documents["acme/mesh"], map[string]any{"slug": "agents", "title": "AGENTS.md", "kind": "instructions", "repo": "acme/widgets", "path": "docs/AGENTS.md"})
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "AGENTS.md"), []byte("root instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "docs", "AGENTS.md"), []byte("docs instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Repos: widgets, Checkouts: map[string]string{"acme/widgets": checkout}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, _, err := WriteContext(dir, cx); err != nil {
		t.Fatal(err)
	}
	cdir := filepath.Join(dir, ContextDir)
	for name, want := range map[string]string{
		"instructions-acme-widgets-agents.md":      "root instructions\n",
		"instructions-acme-widgets-docs-agents.md": "docs instructions\n",
	} {
		got, err := os.ReadFile(filepath.Join(cdir, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q, %v (on disk %v, dropped %v)", name, got, err, cx.OnDisk, slugsOf(cx.Dropped))
		}
		if cx.OnDisk[name].Path == "" {
			t.Fatalf("%s not on disk by path: %v", name, cx.OnDisk)
		}
		index, _ := os.ReadFile(filepath.Join(cdir, "INDEX.md"))
		if !hasLine(string(index), "- `"+name+"` — the instructions of acme/widgets, binding") {
			t.Fatalf("INDEX.md lacks %s:\n%s", name, index)
		}
	}
}

// A path whose name another path of the same repo already took gets a
// name of its own.
func TestContextFileNameNeverCollides(t *testing.T) {
	taken := map[string]gateway.Document{}
	a := gateway.Document{Slug: "agents", Kind: "instructions", Repo: "acme/widgets", Path: "docs/agents.md"}
	b := gateway.Document{Slug: "agents", Kind: "instructions", Repo: "acme/widgets", Path: "docs-agents.md"}
	na := contextFileName(a, taken)
	taken[na] = a
	nb := contextFileName(b, taken)
	if na != "instructions-acme-widgets-docs-agents.md" || nb == na || contextFileName(a, taken) != na {
		t.Fatalf("names %q, %q", na, nb)
	}
}
