package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// The document listing is fetched from /mesh/v1 first; /prologue/v1, the
// dated alias (mesh-specs trd §6c, gone 2026-10-28), is never asked when
// /mesh/v1 serves it.
func TestContextDocumentsUsesMeshV1(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Documents["acme/widgets"] = []map[string]any{{"slug": "agents", "title": "AGENTS", "kind": "instructions"}}
	gw := gateway.New(f.URL(), "rs_test")

	docs, err := gw.ContextDocuments(context.Background(), "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Slug != "agents" {
		t.Fatalf("docs: %+v", docs)
	}
	if f.MeshV1Hits != 1 || f.AliasV1Hits != 0 {
		t.Fatalf("mesh hits = %d, prologue hits = %d; want 1, 0", f.MeshV1Hits, f.AliasV1Hits)
	}
}

// A 404 from /mesh/v1 — an older platform serving only the alias — is
// followed by the same path under /prologue/v1.
func TestContextDocumentsFallsBackToPrologueOn404(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.NoMeshV1 = true
	f.Documents["acme/widgets"] = []map[string]any{{"slug": "agents", "title": "AGENTS", "kind": "instructions"}}
	f.Bodies["acme/widgets/agents"] = "# AGENTS"
	gw := gateway.New(f.URL(), "rs_test")

	docs, err := gw.ContextDocuments(context.Background(), "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Slug != "agents" {
		t.Fatalf("docs: %+v", docs)
	}
	if f.MeshV1Hits != 1 || f.AliasV1Hits != 1 {
		t.Fatalf("mesh hits = %d, prologue hits = %d; want 1, 1", f.MeshV1Hits, f.AliasV1Hits)
	}

	body, err := gw.ContextDocument(context.Background(), "acme", "widgets", "agents")
	if err != nil {
		t.Fatal(err)
	}
	if body != "# AGENTS\n" {
		t.Fatalf("body: %q", body)
	}
	if f.MeshV1Hits != 2 || f.AliasV1Hits != 2 {
		t.Fatalf("mesh hits = %d, prologue hits = %d; want 2, 2", f.MeshV1Hits, f.AliasV1Hits)
	}
}
