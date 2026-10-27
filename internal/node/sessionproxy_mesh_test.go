package node

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/sessionproxy"
	"projectmesh.io/mesh-cli/internal/tools"
)

// The Mesh server is reached as the built-in is: the session's own
// bearer and key, on /mesh/mcp (runsheet ADR 069 step 4).
func TestUpstreamsOfMesh(t *testing.T) {
	specs := map[string]tools.UpstreamSpec{tools.Builtin: {URL: "https://api.runsheet.dev/mcp"}}
	tools.WithMesh(nil, specs, "https://api.runsheet.dev/mcp", nil)
	ups := upstreamsOf(specs, "https://api.runsheet.dev/mcp", nil, nil)
	m, ok := ups[tools.Mesh].(sessionproxy.Runsheet)
	if !ok {
		t.Fatalf("mesh upstream %T, want sessionproxy.Runsheet", ups[tools.Mesh])
	}
	if m.URL != "https://api.runsheet.dev/mesh/mcp" {
		t.Fatalf("mesh upstream at %q", m.URL)
	}
	if b, _ := ups[tools.Builtin].(sessionproxy.Runsheet); b.URL != "https://api.runsheet.dev/mcp" {
		t.Fatalf("built-in at %q", b.URL)
	}
}
