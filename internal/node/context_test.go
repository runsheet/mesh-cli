package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// A design document past the size budget stays in the index and is
// fetched on demand; an ADR past doc_bytes_max is binding and on disk
// while the total holds it.
func TestContextSizeBudget(t *testing.T) {
	f := seed(t)
	f.Documents["acme/mesh"] = append(f.Documents["acme/mesh"], map[string]any{"slug": "trd", "title": "TRD", "kind": "trd", "repo": "acme/widgets", "path": "specs/trd.md"})
	f.Bodies["acme/mesh/trd"] = strings.Repeat("x", DefaultDocBytesMax+1)
	f.Bodies["acme/mesh/adr-001"] = strings.Repeat("y", DefaultDocBytesMax+1) // binding: on disk whatever its size
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cx.Docs["trd-trd.md"]; ok {
		t.Fatal("an oversized TRD must not be on disk")
	}
	if _, ok := cx.Docs["adr-adr-001.md"]; !ok {
		t.Fatal("an ADR is on disk whatever its size")
	}
	if cx.Project != "acme/mesh" || len(cx.Index) != 5 {
		t.Fatalf("context: project %s, %d in the index", cx.Project, len(cx.Index))
	}
	dir := t.TempDir()
	names, _, err := WriteContext(dir, cx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "instructions-acme-widgets-agents.md,adr-adr-001.md,prd-prd.md" {
		t.Fatalf("written: %v", names)
	}
	index, _ := os.ReadFile(filepath.Join(dir, ContextDir, IndexFile))
	if !strings.Contains(string(index), "| trd | TRD |  | `trd` | acme/widgets/specs/trd.md | fetch |") || !strings.Contains(string(index), "`mesh_document`") {
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

// RenderIndex carries a summary only for a ContextKinds document that is
// not on disk; an on-disk ADR gets no summary cell, and a kind a session
// rarely fetches (a skill) is a row of the second table, kind, title and
// slug alone, even when the document has one.
func TestRenderIndexSummaries(t *testing.T) {
	f := seed(t)
	f.Documents["acme/mesh"][0]["summary"] = "What the ADR decides." // adr-001, on disk
	f.Documents["acme/mesh"] = append(f.Documents["acme/mesh"],
		map[string]any{"slug": "trd", "title": "TRD", "kind": "trd", "summary": "The design in one line.", "repo": "acme/widgets", "path": "specs/trd.md"},
		map[string]any{"slug": "deploy", "title": "Deploy", "kind": "skill", "summary": "How to deploy.", "repo": "acme/widgets", "path": "skills/deploy.md"},
	)
	f.Bodies["acme/mesh/trd"] = strings.Repeat("x", DefaultDocBytesMax+1) // too big to land on disk
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cx.Index) != 6 {
		t.Fatalf("index: %d documents", len(cx.Index))
	}
	index := RenderIndex(cx)
	lines := strings.Split(index, "\n")
	lineSet := make(map[string]bool, len(lines))
	for _, l := range lines {
		lineSet[l] = true
	}
	want := []string{
		"| adr | ADR 001 |  | `adr-001` | acme/widgets/specs/adr-001.md | `adr-adr-001.md` |",
		"| trd | TRD | The design in one line. | `trd` | acme/widgets/specs/trd.md | fetch |",
		"| skill | Deploy | `deploy` |",
		"3 documents on disk here, 3 to fetch (1 a session reads, 2 other); summaries are shown only for the kinds a session reads (instructions, adr, prd, trd) that are not on disk.",
	}
	for _, line := range want {
		if !lineSet[line] {
			t.Fatalf("index missing exact line %q:\n%s", line, index)
		}
	}
	if bodyRows := indexRows(index); bodyRows != len(cx.Index) {
		t.Fatalf("rendered %d body rows, want %d (one per document):\n%s", bodyRows, len(cx.Index), index)
	}
}

// indexRows is how many body rows INDEX.md's documents tables hold:
// every table line but the headers and separators, the live section's
// tables not among them.
func indexRows(index string) int {
	docs, _, _ := strings.Cut(index, "\n## Live goals and items")
	rows := 0
	for _, l := range strings.Split(docs, "\n") {
		switch l {
		case indexReadsHeader, indexOtherHeader, "| --- | --- | --- | --- | --- | --- |", "| --- | --- | --- |":
		default:
			if strings.HasPrefix(l, "| ") {
				rows++
			}
		}
	}
	return rows
}

const (
	indexReadsHeader = "| kind | title | what it says | slug | repo/path | here |"
	indexOtherHeader = "| kind | title | slug |"
)

// INDEX.md is two tables: the kinds a session reads with summary, path
// and where it is, and every other kind by kind, title and slug alone;
// the counts line names both.
func TestRenderIndexTwoTables(t *testing.T) {
	cx := &Context{
		Project: "acme/mesh",
		Index: []gateway.Document{
			{Slug: "agents", Title: "AGENTS.md", Kind: "instructions", Summary: "How to work here.", Repo: "acme/widgets", Path: "AGENTS.md"},
			{Slug: "adr-002", Title: "ADR 002", Kind: "adr", Summary: "Tags are signed.", Repo: "acme/widgets", Path: "docs/adr-002.md"},
			{Slug: "restart", Title: "Restart | the node", Kind: "runbook", Summary: "Never shown.", Repo: "acme/widgets", Path: "runbooks/restart.md"},
			{Slug: "deploy", Title: "Deploy", Kind: "skill", Summary: "Never shown.", Repo: "acme/widgets", Path: "skills/deploy.md"},
		},
		OnDisk: map[string]string{"agents": "instructions-acme-widgets-agents.md"},
	}
	index := RenderIndex(cx)
	count := func(want string) int {
		n := 0
		for _, l := range strings.Split(index, "\n") {
			if l == want {
				n++
			}
		}
		return n
	}
	for _, want := range []string{
		"## Documents a session reads",
		indexReadsHeader,
		"| instructions | AGENTS.md |  | `agents` | acme/widgets/AGENTS.md | `instructions-acme-widgets-agents.md` |",
		"| adr | ADR 002 | Tags are signed. | `adr-002` | acme/widgets/docs/adr-002.md | fetch |",
		"## Other documents",
		indexOtherHeader,
		"| runbook | Restart \\| the node | `restart` |",
		"| skill | Deploy | `deploy` |",
		"1 documents on disk here, 3 to fetch (1 a session reads, 2 other); summaries are shown only for the kinds a session reads (instructions, adr, prd, trd) that are not on disk.",
	} {
		if got := count(want); got != 1 {
			t.Fatalf("INDEX.md holds the line %q %d times, want once:\n%s", want, got, index)
		}
	}
	if strings.Contains(index, "Never shown.") {
		t.Fatalf("a runbook's or a skill's summary is in the index:\n%s", index)
	}
	if rows := indexRows(index); rows != len(cx.Index) {
		t.Fatalf("%d rows, want one per document:\n%s", rows, index)
	}
}

// INDEX.md is inside bytes_max: several hundred documents of mixed kinds
// under a budget smaller than the index and the documents together put
// on disk no more than bytes_max, INDEX.md counted; the item's own
// repo's instructions are on disk, every document is one row, and
// nothing on disk is cut.
func TestIndexIsInsideTheBudget(t *testing.T) {
	f := seed(t)
	type doc struct{ kind, slug, repo string }
	var docs []doc
	docs = append(docs, doc{"instructions", "agents", "acme/widgets"}, doc{"prd", "prd", "acme/widgets"}, doc{"trd", "trd", "acme/widgets"})
	for i := 1; i <= 40; i++ {
		docs = append(docs, doc{"instructions", fmt.Sprintf("agents-%03d", i), fmt.Sprintf("acme/repo-%03d", i)})
	}
	for i := 1; i <= 80; i++ {
		repo := fmt.Sprintf("acme/repo-%03d", i%40+1)
		if i%10 == 0 {
			repo = "acme/widgets"
		}
		docs = append(docs, doc{"adr", fmt.Sprintf("adr-%03d", i), repo})
	}
	for i := 1; i <= 200; i++ {
		kind := []string{"runbook", "skill", "spec", "note"}[i%4]
		docs = append(docs, doc{kind, fmt.Sprintf("%s-%03d", kind, i), "acme/widgets"})
	}
	f.Documents["acme/mesh"] = nil
	bodies := map[string]string{}
	full := 0
	for i, d := range docs {
		f.Documents["acme/mesh"] = append(f.Documents["acme/mesh"], map[string]any{
			"slug": d.slug, "title": "Title of " + d.slug, "kind": d.kind, "repo": d.repo, "path": d.slug + ".md",
			"summary": "What " + d.slug + " says, in one line of about sixty bytes or so.",
		})
		if !ContextKinds[d.kind] {
			continue
		}
		body := "# " + d.slug + "\n" + strings.Repeat("z", 400+(i%7)*150)
		f.Bodies["acme/mesh/"+d.slug] = body
		bodies[d.slug] = body + "\n" // the fake serves a newline after it
		full += len(bodies[d.slug])
	}
	if len(docs) < 300 {
		t.Fatalf("%d documents, want at least 300", len(docs))
	}
	gw := gateway.New(f.URL(), "rs_test")
	const bytesMax = 48 * 1024
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: bytesMax}, Repos: []string{"acme/widgets"}})
	if err != nil {
		t.Fatal(err)
	}
	if full+len(RenderIndex(&Context{Index: cx.Index, Project: cx.Project})) <= bytesMax {
		t.Fatal("the budget must be smaller than the index and the documents together")
	}
	dir := t.TempDir()
	_, w, err := WriteContext(dir, cx)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	entries, _ := os.ReadDir(filepath.Join(dir, ContextDir))
	for _, e := range entries {
		st, err := os.Stat(filepath.Join(dir, ContextDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		total += int(st.Size())
	}
	if total > bytesMax || w.Bytes != total {
		t.Fatalf("%d bytes on disk (logged %d), bytes_max %d", total, w.Bytes, bytesMax)
	}
	if cx.OnDisk["agents"] != "instructions-acme-widgets-agents.md" {
		t.Fatalf("the item's own repo's instructions must be on disk: %v", cx.OnDisk)
	}
	if len(cx.Dropped) == 0 || len(cx.OnDisk) < 2 {
		t.Fatalf("want some binding documents on disk and some dropped: %d on disk, %d dropped", len(cx.OnDisk), len(cx.Dropped))
	}
	b, _ := os.ReadFile(filepath.Join(dir, ContextDir, IndexFile))
	index := string(b)
	if cx.IndexBytes != len(index) {
		t.Fatalf("IndexBytes %d, INDEX.md %d bytes", cx.IndexBytes, len(index))
	}
	if rows := indexRows(index); rows != len(docs) {
		t.Fatalf("%d rows in INDEX.md, want %d, one per document", rows, len(docs))
	}
	for _, d := range cx.Index {
		n := 0
		for _, l := range strings.Split(index, "\n") {
			if strings.Contains(l, " | `"+d.Slug+"` |") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s is %d rows of INDEX.md, want 1", d.Slug, n)
		}
	}
	for _, d := range cx.Dropped {
		row := fmt.Sprintf("| %s | Title of %s | What %s says, in one line of about sixty bytes or so. | `%s` | %s/%s.md | fetch |", d.Kind, d.Slug, d.Slug, d.Slug, d.Repo, d.Slug)
		if !hasLine(index, row) {
			t.Fatalf("dropped %s is not a whole row to fetch:\n%s", d.Slug, row)
		}
	}
	for slug, name := range cx.OnDisk {
		got, err := os.ReadFile(filepath.Join(dir, ContextDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != bodies[slug] {
			t.Fatalf("%s on disk is %d bytes, its document %d: cut", name, len(got), len(bodies[slug]))
		}
	}
}

// An index alone past bytes_max puts no document on disk and says so
// once; the index is whole.
func TestIndexAlonePastTheBudget(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: 100}, Repos: []string{"acme/widgets"}, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	if len(cx.OnDisk) != 0 || len(cx.Docs) != 0 {
		t.Fatalf("on disk: %v", cx.OnDisk)
	}
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "warning: INDEX.md alone is ") {
		t.Fatalf("logged %q", logged)
	}
	if got := strings.Join(slugsOf(cx.Dropped), ","); got != "adr-001,agents" {
		t.Fatalf("dropped: %s", got)
	}
	if rows := indexRows(RenderIndex(cx)); rows != len(cx.Index) {
		t.Fatalf("%d rows, want %d", rows, len(cx.Index))
	}
}

// indexReserve is the length of the seeded project's INDEX.md with
// nothing on disk, what FetchContext reserves from bytes_max first.
func indexReserve(t *testing.T, gw *gateway.Client) int {
	t.Helper()
	docs, err := gw.ContextDocuments(context.Background(), "acme", "mesh")
	if err != nil {
		t.Fatal(err)
	}
	return len(RenderIndex(&Context{Index: docs, Project: "acme/mesh"}))
}

// bindingDoc is one binding document of the project for the seeded
// fake: its slug, kind, repo and a body of n bytes that says whose it is.
type bindingDoc struct {
	kind, slug, repo string
	n                int
}

// seedBinding replaces the seeded project's documents with docs and
// returns each body, n bytes as served, by slug.
func seedBinding(f *gatewaytest.Fake, docs ...bindingDoc) map[string]string {
	bodies := map[string]string{}
	f.Documents["acme/mesh"] = nil
	for _, d := range docs {
		f.Documents["acme/mesh"] = append(f.Documents["acme/mesh"], map[string]any{"slug": d.slug, "title": d.slug, "kind": d.kind, "repo": d.repo, "path": d.slug + ".md"})
		head := "# " + d.slug + " of " + d.repo + "\n"
		bodies[d.slug] = head + strings.Repeat("b", d.n-len(head)-1) + "\n"
		// the fake serves a body with a newline after it
		f.Bodies["acme/mesh/"+d.slug] = strings.TrimSuffix(bodies[d.slug], "\n")
	}
	return bodies
}

// onDiskSlack is room past the reserve for the on-disk rows' file names
// in the budget tests below (INDEX.md is inside bytes_max): less than
// any one document there, so what fits is decided by the documents.
const onDiskSlack = 140

// slugsOf is the documents' slugs, sorted.
func slugsOf(docs []gateway.Document) []string {
	var out []string
	for _, d := range docs {
		out = append(out, d.Slug)
	}
	sort.Strings(out)
	return out
}

// onDisk is cx's on-disk slugs, sorted.
func onDisk(cx *Context) []string {
	var out []string
	for s := range cx.OnDisk {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// More binding documents than bytes_max holds: the item's own repo's
// instructions and ADR are on disk, then the most recent other ADR; the
// rest are dropped whole, listed in INDEX.md to fetch, and nothing on
// disk is cut.
func TestContextBudgetHoldsForBindingDocuments(t *testing.T) {
	f := seed(t)
	bodies := seedBinding(f,
		bindingDoc{"adr", "adr-002", "acme/other", 1000},
		bindingDoc{"adr", "adr-001", "acme/widgets", 1000},
		bindingDoc{"adr", "adr-003", "acme/third", 1000},
		bindingDoc{"instructions", "agents", "acme/widgets", 1000},
		bindingDoc{"adr", "adr-004", "acme/other", 1000},
	)
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: 3000 + indexReserve(t, gw) + onDiskSlack}, Repos: []string{"acme/widgets"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(onDisk(cx), ","); got != "adr-001,adr-004,agents" {
		t.Fatalf("on disk: %s, want the widgets instructions and ADR and adr-004", got)
	}
	if got := strings.Join(slugsOf(cx.Dropped), ","); got != "adr-002,adr-003" {
		t.Fatalf("dropped: %s", got)
	}
	for slug, name := range cx.OnDisk {
		if cx.Docs[name] != bodies[slug] {
			t.Fatalf("%s on disk is not the seeded body: %d bytes, want %d", name, len(cx.Docs[name]), len(bodies[slug]))
		}
	}
	index := RenderIndex(cx)
	for _, row := range []string{
		"| adr | adr-002 |  | `adr-002` | acme/other/adr-002.md | fetch |",
		"| adr | adr-003 |  | `adr-003` | acme/third/adr-003.md | fetch |",
		"| adr | adr-004 |  | `adr-004` | acme/other/adr-004.md | `adr-adr-004.md` |",
		"| instructions | agents |  | `agents` | acme/widgets/agents.md | `instructions-acme-widgets-agents.md` |",
	} {
		if !hasLine(index, row) {
			t.Fatalf("INDEX.md lacks the line\n%s\nin\n%s", row, index)
		}
	}
}

// A budget the item's own repo's documents fill leaves every other
// repo's to the index.
func TestContextBudgetOwnRepoFirst(t *testing.T) {
	f := seed(t)
	seedBinding(f,
		bindingDoc{"adr", "adr-009", "acme/other", 1000},
		bindingDoc{"adr", "adr-001", "acme/widgets", 1000},
		bindingDoc{"instructions", "other-agents", "acme/other", 1000},
		bindingDoc{"instructions", "agents", "acme/widgets", 1000},
	)
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: 2000 + indexReserve(t, gw) + onDiskSlack}, Repos: []string{"acme/widgets"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(onDisk(cx), ","); got != "adr-001,agents" {
		t.Fatalf("on disk: %s, want the widgets documents alone", got)
	}
	if got := strings.Join(slugsOf(cx.Dropped), ","); got != "adr-009,other-agents" {
		t.Fatalf("dropped: %s", got)
	}
}

// Binding documents alone past the budget still make a usable context,
// led by the item's own repo; a document too big for what is left is
// skipped and a smaller one after it still lands.
func TestContextBudgetBindingAloneExceeds(t *testing.T) {
	f := seed(t)
	seedBinding(f,
		bindingDoc{"adr", "adr-010", "acme/other", 5000},
		bindingDoc{"adr", "adr-011", "acme/third", 5000},
		bindingDoc{"instructions", "agents", "acme/widgets", 500},
		bindingDoc{"adr", "adr-001", "acme/widgets", 500},
		bindingDoc{"adr", "adr-002", "acme/third", 150},
		bindingDoc{"instructions", "other-agents", "acme/other", 4000},
	)
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: 1200 + indexReserve(t, gw) + onDiskSlack}, Repos: []string{"acme/widgets"}})
	if err != nil {
		t.Fatal(err)
	}
	if cx.OnDisk["agents"] != "instructions-acme-widgets-agents.md" || cx.OnDisk["adr-001"] != "adr-adr-001.md" {
		t.Fatalf("the item's own repo's instructions and ADR must be on disk: %v", cx.OnDisk)
	}
	if got := strings.Join(onDisk(cx), ","); got != "adr-001,adr-002,agents" {
		t.Fatalf("on disk: %s", got)
	}
	if got := strings.Join(slugsOf(cx.Dropped), ","); got != "adr-010,adr-011,other-agents" {
		t.Fatalf("dropped: %s", got)
	}
}

// Recency is the slug's number: adr-014 before adr-001, whatever
// follows it.
func TestSlugNumber(t *testing.T) {
	for slug, want := range map[string]int{"adr-014": 14, "adr-014-a-build-validates": 14, "adr-001": 1, "agents": -1} {
		if got := slugNumber(slug); got != want {
			t.Errorf("slugNumber(%q) = %d, want %d", slug, got, want)
		}
	}
}

// WriteContext's ContextWritten is what is on disk: the bytes of every
// document file and INDEX.md as read back, the document count, and the
// index rows with no file (adr-016).
func TestContextWritten(t *testing.T) {
	f := seed(t)
	f.Documents["acme/mesh"] = append(f.Documents["acme/mesh"], map[string]any{"slug": "trd", "title": "TRD", "kind": "trd", "repo": "acme/widgets", "path": "specs/trd.md"})
	f.Bodies["acme/mesh/trd"] = strings.Repeat("x", DefaultDocBytesMax+1)
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{})
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
