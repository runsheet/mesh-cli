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
