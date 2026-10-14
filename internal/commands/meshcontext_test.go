package commands

import (
	"context"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// A node's session gets mesh-context as this binary's mcp-relay to the
// gateway's /mcp, the token file left for the harness to place and the
// bearer nowhere in the entry (ADR-029); tokenKeeper is nil, not a nil
// pointer, with no sessionTokens.
func TestMeshContextMCPIsThisBinarysRelay(t *testing.T) {
	gw := gateway.New("https://api.test/", "rs_node_bearer")
	srv, _ := meshContextMCP(context.Background(), gw)[harness.BuiltinMCP].(map[string]any)
	if srv == nil {
		t.Fatal("no mesh-context")
	}
	args, _ := srv["args"].([]string)
	if srv["type"] != "stdio" || srv["command"] != meshBinary() ||
		!slices.Equal(args, []string{harness.RelayCommand, "--url", "https://api.test/mcp", "--token-file", ""}) {
		t.Fatalf("mesh-context = %v", srv)
	}
	if strings.Contains(strings.Join(args, " "), "rs_node_bearer") || srv["headers"] != nil {
		t.Fatalf("the bearer rides the entry: %v", srv)
	}
	saved := sessionTokens
	t.Cleanup(func() { sessionTokens = saved })
	sessionTokens = nil
	if tokenKeeper() != nil {
		t.Fatal("tokenKeeper with no sessionTokens is not nil")
	}
}
