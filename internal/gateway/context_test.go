package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// Documents reads the mesh-context server at /mesh/v1, not its dated
// alias.
func TestDocumentsHitsMeshV1(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Documents["acme/mesh"] = []map[string]any{{"slug": "trd", "title": "TRD", "kind": "trd"}}
	gw := gateway.New(f.URL(), "rs_test")

	docs, err := gw.Documents(context.Background(), "acme", "mesh")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Slug != "trd" {
		t.Fatalf("docs = %+v", docs)
	}
	if len(f.ContextPaths) != 1 || f.ContextPaths[0] != "/mesh/v1/projects/acme/mesh/documents" {
		t.Fatalf("context paths = %v, want one hit on /mesh/v1", f.ContextPaths)
	}
}

// A platform old enough to answer /mesh/v1 with 404 still serves
// mesh-context: the client retries once under /prologue/v1 and, having
// learned that, goes straight there from then on.
func TestDocumentsFallsBackToPrologueV1(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NoMeshV1 = true
	f.Documents["acme/mesh"] = []map[string]any{{"slug": "trd", "title": "TRD", "kind": "trd"}}
	gw := gateway.New(f.URL(), "rs_test")

	docs, err := gw.Documents(context.Background(), "acme", "mesh")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Slug != "trd" {
		t.Fatalf("docs = %+v", docs)
	}
	want := []string{"/mesh/v1/projects/acme/mesh/documents", "/prologue/v1/projects/acme/mesh/documents"}
	if len(f.ContextPaths) != 2 || f.ContextPaths[0] != want[0] || f.ContextPaths[1] != want[1] {
		t.Fatalf("context paths = %v, want %v", f.ContextPaths, want)
	}

	// the choice is remembered: a second read goes straight to /prologue/v1
	if _, err := gw.Documents(context.Background(), "acme", "mesh"); err != nil {
		t.Fatal(err)
	}
	if len(f.ContextPaths) != 3 || f.ContextPaths[2] != want[1] {
		t.Fatalf("context paths = %v, want a third hit on /prologue/v1 alone", f.ContextPaths)
	}
}
