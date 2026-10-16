package gateway_test

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// The document listing is fetched from /mesh/v1, the only path the
// gateway serves.
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
	if f.MeshV1Hits != 1 {
		t.Fatalf("mesh hits = %d; want 1", f.MeshV1Hits)
	}
}

// A 404 from /mesh/v1 is an ordinary error, with no second request.
func TestContextDocument404IsAnError(t *testing.T) {
	f := gatewaytest.New("rs_test")
	defer f.Close()
	f.Documents["acme/widgets"] = []map[string]any{{"slug": "agents", "title": "AGENTS", "kind": "instructions"}}
	gw := gateway.New(f.URL(), "rs_test")

	_, err := gw.ContextDocument(context.Background(), "acme", "widgets", "missing")
	if err == nil {
		t.Fatal("want an error for a document the fake does not have")
	}
	if f.MeshV1Hits != 1 {
		t.Fatalf("mesh hits = %d; want 1 (no retry)", f.MeshV1Hits)
	}
}
