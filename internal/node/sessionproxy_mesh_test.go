package node

import (
	"testing"

	"projectmesh.io/mesh-cli/internal/sessionproxy"
	"projectmesh.io/mesh-cli/internal/tools"
)

// A server the job's brief names is reached as the built-in is: the
// session's own bearer and key, at the URL the brief gives.
func TestUpstreamsOfBriefServer(t *testing.T) {
	specs := map[string]tools.UpstreamSpec{tools.Builtin: {URL: "https://platform.test/mcp"}}
	added := tools.WithServers(nil, specs, []tools.Server{{Name: "jobs", URL: "https://platform.test/jobs/mcp"}}, nil)
	if len(added) != 1 || added[0] != "jobs" {
		t.Fatalf("added %v", added)
	}
	ups := upstreamsOf(specs, "https://platform.test/mcp", nil, nil)
	m, ok := ups["jobs"].(sessionproxy.Runsheet)
	if !ok {
		t.Fatalf("jobs upstream %T, want the session's own bearer (sessionproxy.Runsheet)", ups["jobs"])
	}
	if m.URL != "https://platform.test/jobs/mcp" {
		t.Fatalf("jobs upstream at %q", m.URL)
	}
}
