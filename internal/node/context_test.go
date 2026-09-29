package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A design document past the size budget stays in the index and is
// fetched on demand; instructions and ADRs are always on disk.
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

// RenderIndex carries a summary only for a ContextKinds document that is
// not on disk; an on-disk ADR and a kind a session rarely fetches (a
// skill) get no summary cell even when the document has one.
func TestRenderIndexSummaries(t *testing.T) {
	f := seed(t)
	f.Documents["acme/mesh"][0]["summary"] = "What the ADR decides." // adr-001, on disk
	f.Documents["acme/mesh"] = append(f.Documents["acme/mesh"],
		map[string]any{"slug": "trd", "title": "TRD", "kind": "trd", "summary": "The design in one line.", "repo": "acme/widgets", "path": "specs/trd.md"},
		map[string]any{"slug": "deploy", "title": "Deploy", "kind": "skill", "summary": "How to deploy.", "repo": "acme/widgets", "path": "skills/deploy.md"},
	)
	f.Bodies["acme/mesh/trd"] = strings.Repeat("x", DocBytesMax+1) // too big to land on disk
	gw := gateway.New(f.URL(), "rs_test")
	cx, err := FetchContext(context.Background(), gw, "acme", "mesh")
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
		"| skill | Deploy |  | `deploy` | acme/widgets/skills/deploy.md | fetch |",
		"3 documents on disk here, 3 to fetch; summaries are shown only for the kinds a session reads (instructions, adr, prd, trd) that are not on disk.",
	}
	for _, line := range want {
		if !lineSet[line] {
			t.Fatalf("index missing exact line %q:\n%s", line, index)
		}
	}
	header := "| kind | title | what it says | slug | repo/path | here |"
	separator := "| --- | --- | --- | --- | --- | --- |"
	bodyRows := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "| ") && l != header && l != separator {
			bodyRows++
		}
	}
	if bodyRows != len(cx.Index) {
		t.Fatalf("rendered %d body rows, want %d (one per document):\n%s", bodyRows, len(cx.Index), index)
	}
}
