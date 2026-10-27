package harness

import (
	"slices"
	"testing"
)

// A build session with the Mesh server may not call its mesh_fail or
// mesh_release (the node's own, runsheet ADR 069); one without it is
// told nothing about a server it does not have.
func TestHostedDisallowedNamesTheMeshServerOnlyWhenPresent(t *testing.T) {
	without := HostedDisallowed(Job{Kind: KindBuild, MCPServers: map[string]any{BuiltinMCP: map[string]any{}}})
	if slices.Contains(without, "mcp__mesh__mesh_fail") {
		t.Fatalf("no mesh server, yet %v", without)
	}
	with := HostedDisallowed(Job{Kind: KindBuild, MCPServers: map[string]any{BuiltinMCP: map[string]any{}, MeshMCP: map[string]any{}}})
	for _, want := range []string{"mcp__runsheet__mesh_finish", "mcp__runsheet__mesh_fail", "mcp__mesh__mesh_fail", "mcp__mesh__mesh_release"} {
		if !slices.Contains(with, want) {
			t.Fatalf("%v lacks %s", with, want)
		}
	}
	if slices.Contains(with, "mcp__mesh__mesh_progress") {
		t.Fatalf("a build session's own mesh_progress disallowed: %v", with)
	}
}
