package harness

import (
	"slices"
	"testing"

	"projectmesh.io/mesh-cli/internal/tools"
)

// A build session may not call mesh_fail or mesh_release on a server its
// brief names (the node's own); a session without such a server is told
// nothing of one.
func TestHostedDisallowedNamesTheBriefsServers(t *testing.T) {
	without := HostedDisallowed(Job{Kind: KindBuild, MCPServers: map[string]any{BuiltinMCP: map[string]any{}}})
	if !slices.Equal(without, HostedFinishTools) {
		t.Fatalf("no brief server, yet %v", without)
	}
	job := Job{Kind: KindBuild,
		MCPServers:   map[string]any{BuiltinMCP: map[string]any{}, "jobs": map[string]any{}},
		BriefServers: []tools.Server{{Name: "jobs", URL: "https://example.test/jobs/mcp"}}}
	with := HostedDisallowed(job)
	for _, want := range []string{"mcp__runsheet__mesh_finish", "mcp__runsheet__mesh_fail", "mcp__jobs__mesh_fail", "mcp__jobs__mesh_release"} {
		if !slices.Contains(with, want) {
			t.Fatalf("%v lacks %s", with, want)
		}
	}
	if slices.Contains(with, "mcp__jobs__mesh_progress") {
		t.Fatalf("a build session's own mesh_progress disallowed: %v", with)
	}
}
