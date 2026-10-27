package tools

import "testing"

func TestMeshURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.runsheet.dev/mcp":  "https://api.runsheet.dev/mesh/mcp",
		"https://api.runsheet.dev/mcp/": "https://api.runsheet.dev/mesh/mcp",
		"http://127.0.0.1:8080/mcp":     "http://127.0.0.1:8080/mesh/mcp",
		"https://api.runsheet.dev":      "",
		"":                              "",
	} {
		if got := MeshURL(in); got != want {
			t.Errorf("MeshURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// WithMesh adds the Mesh server beside the built-in, on the same shim,
// and leaves a server already named mesh as it is.
func TestWithMesh(t *testing.T) {
	shim := func(name string) map[string]any { return map[string]any{"server": name} }
	servers, ups, err := ProxiedServers("https://api.runsheet.dev/mcp", nil, shim)
	if err != nil {
		t.Fatal(err)
	}
	WithMesh(servers, ups, "https://api.runsheet.dev/mcp", shim)
	if got := ups[Mesh].URL; got != "https://api.runsheet.dev/mesh/mcp" {
		t.Fatalf("mesh upstream %q", got)
	}
	if s, _ := servers[Mesh].(map[string]any); s["server"] != Mesh {
		t.Fatalf("mesh server %v", servers[Mesh])
	}
	if ups[Builtin].URL != "https://api.runsheet.dev/mcp" {
		t.Fatalf("built-in moved: %v", ups[Builtin])
	}

	taken := map[string]UpstreamSpec{Mesh: {Transport: Remote, URL: "https://example.com/mcp"}}
	WithMesh(nil, taken, "https://api.runsheet.dev/mcp", shim)
	if taken[Mesh].URL != "https://example.com/mcp" {
		t.Fatalf("a tool named mesh was replaced: %v", taken[Mesh])
	}
	none := map[string]UpstreamSpec{}
	WithMesh(nil, none, "https://api.runsheet.dev", shim)
	if _, ok := none[Mesh]; ok {
		t.Fatal("mesh added beside no /mcp")
	}
}
