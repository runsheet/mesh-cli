package node

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

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

// Reloader carries node.yaml's `context:` as it now stands.
func TestReloaderCarriesContext(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", Capacity: Fixed(1)}
	load := func() (*Config, error) {
		c := *cfg
		c.Context = ContextConfig{BytesMax: 131072}
		return &c, nil
	}
	reload := Reloader(gw, cfg, scopesOf(cfg), load, func(context.Context, Scope) error { return nil })
	got, _ := reload(context.Background())
	if got.Context == nil || *got.Context != (ContextConfig{BytesMax: 131072}) {
		t.Fatalf("Reloaded.Context = %+v, want bytes_max 131072", got.Context)
	}
}

// The context budget a Reload returns is in force after the next tick,
// with no restart, and the change is logged.
func TestLoopContextBudgetIsLiveReloaded(t *testing.T) {
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", NodeID: "22222222-2222-4222-8222-222222222222", Capacity: Fixed(1)}
	var logs strings.Builder
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Interval: time.Hour, Log: log.New(&logs, "", 0),
		Dispatch: func(context.Context, gateway.WorkItem) {}}
	if got := loop.ContextBudget(); got != (ContextConfig{BytesMax: DefaultContextBytesMax, DocBytesMax: DefaultDocBytesMax}) {
		t.Fatalf("before reload: %+v, want the defaults", got)
	}
	loop.Reload = func(context.Context) (Reloaded, error) {
		return Reloaded{Context: &ContextConfig{BytesMax: 4096, DocBytesMax: 1024}}, nil
	}
	loop.Tick(context.Background())
	loop.wg.Wait()
	if got := loop.ContextBudget(); got != (ContextConfig{BytesMax: 4096, DocBytesMax: 1024}) {
		t.Fatalf("after reload: %+v, want 4096/1024", got)
	}
	if !strings.Contains(logs.String(), "context: bytes_max 81920 → 4096, doc_bytes_max 61440 → 1024") {
		t.Fatalf("the change is not logged:\n%s", logs.String())
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

// hasLine reports whether s holds want as one whole line.
func hasLine(s, want string) bool {
	for _, l := range strings.Split(s, "\n") {
		if l == want {
			return true
		}
	}
	return false
}
