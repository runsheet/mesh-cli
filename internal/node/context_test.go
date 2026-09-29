package node

import (
	"context"
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
	names, err := WriteContext(dir, cx)
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
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: 3000}, Repos: []string{"acme/widgets"}})
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
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: 2999}, Repos: []string{"acme/widgets"}})
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
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{BytesMax: 1200}, Repos: []string{"acme/widgets"}})
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
