package tools_test

import (
	"encoding/json"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/tools"
)

// The built-in renders as runsheet (ADR-006's amendment 2026-10-15): the
// Claude Code config's mcpServers key, its --allowedTools, and Codex's
// config.toml table; mesh-context is nowhere.
func TestBuiltinRendersAsRunsheet(t *testing.T) {
	builtin := map[string]any{harness.BuiltinMCP: map[string]any{"type": "http", "url": "https://api.runsheet.dev/mcp"}}
	servers, _, err := tools.ClaudeCode(builtin, []tools.Tool{{Name: "search", Transport: tools.Remote, URL: "https://search.example/mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	args := harness.MCPArgs(servers)
	var cfg struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if len(args) < 4 || args[0] != "--mcp-config" || args[2] != "--allowedTools" {
		t.Fatalf("args = %v", args)
	}
	if err := json.Unmarshal([]byte(args[1]), &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.MCPServers["runsheet"]; !ok {
		t.Fatalf("mcpServers has no runsheet: %s", args[1])
	}
	if _, ok := cfg.MCPServers["mesh-context"]; ok {
		t.Fatalf("mcpServers still names mesh-context: %s", args[1])
	}
	if !strings.Contains(args[3], "mcp__runsheet__*") || strings.Contains(args[3], "mesh-context") {
		t.Fatalf("--allowedTools = %s", args[3])
	}

	toml, _, err := harness.CodexMCPConfig(builtin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(toml, "[mcp_servers.runsheet]\n") || strings.Contains(toml, "mesh-context") {
		t.Fatalf("config.toml =\n%s", toml)
	}
}
