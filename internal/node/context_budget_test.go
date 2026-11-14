package node

import (
	"context"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A node.yaml without `context:` has the 80KB cap (and doc_bytes_max's 60KB, unused);
// a key left out of `context:` takes its default.
func TestContextConfigDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Context.Effective(); got != (ContextConfig{BytesMax: 81920, DocBytesMax: 61440}) {
		t.Fatalf("no context: %+v, want 81920/61440", got)
	}
	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\ncontext: {bytes_max: 131072}\n")
	if cfg, err = LoadConfig(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Context.Effective(); got != (ContextConfig{BytesMax: 131072, DocBytesMax: 61440}) {
		t.Fatalf("bytes_max only: %+v, want 131072/61440", got)
	}
}

// A negative size is refused, naming the key.
func TestContextConfigNegativeRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for key, yaml := range map[string]string{
		"context.bytes_max":     "context: {bytes_max: -1}\n",
		"context.doc_bytes_max": "context: {doc_bytes_max: -1}\n",
	} {
		writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\n"+yaml)
		if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("%s: err = %v, want one naming the key", key, err)
		}
	}
}

// doc_bytes_max holds each instructions document and nothing else: no
// PRD or ADR is put on disk whatever it is set to, and an instructions
// document past it is left whole to the index.
func TestFetchContextHonoursDocBytesMax(t *testing.T) {
	f := seed(t)
	f.Bodies["acme/mesh/prd"] = strings.Repeat("p", 11)
	f.Bodies["acme/mesh/agents"] = strings.Repeat("a", 11)
	gw := gateway.New(f.URL(), "rs_test")
	for _, docMax := range []int{10, 0, 1 << 20} {
		cx, err := FetchContext(context.Background(), gw, "acme", "mesh", ContextOptions{Budget: ContextConfig{DocBytesMax: docMax}, Repos: widgets})
		if err != nil {
			t.Fatal(err)
		}
		var slugs []string
		for _, d := range cx.OnDisk {
			slugs = append(slugs, d.Slug)
		}
		// past doc_bytes_max the instructions go to the index, whole
		want := "agents"
		if docMax == 10 {
			want = ""
		}
		if strings.Join(slugs, ",") != want {
			t.Fatalf("doc_bytes_max %d: on disk %v", docMax, slugs)
		}
	}
}
