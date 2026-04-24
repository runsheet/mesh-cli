package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// mcpServerEntry is the shape Claude Code expects under mcpServers in
// .claude/settings.json: a command + args list that it spawns with stdio piped
// for JSON-RPC.
type mcpServerEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type claudeSettings struct {
	MCPServers map[string]mcpServerEntry `json:"mcpServers,omitempty"`
}

// WriteClaudeSettings materializes the per-agent plugin under
// ~/.mesh/agents/<agentName>/plugin/. Claude Code is spawned with
// --plugin-dir pointing at that subdir, so the MCP servers register at
// plugin scope (which is the scope where experimental.claude/channel is
// honored). Crucially, no .mcp.json is written to the agent cwd —
// project-scope and plugin-scope same-keyed servers conflict, with
// project-scope winning and plugin-scope getting silently skipped.
//
// All channel types are served through a single "mesh" MCP server key
// backed by the mesh-shim subcommand, which connects to the daemon's
// per-agent router socket and multiplexes across all platform gateways.
//
// envSource is retained for interface compatibility but is no longer used to
// hoist bot tokens — account credentials live in the layer-1 account dir and
// are read by the gateway, never passed through settings.json.
func WriteClaudeSettings(agentName string, channels []ChannelSpec, envSource map[string]string) error {
	dir, err := agentWorkdir(agentName)
	if err != nil {
		return err
	}
	pluginDir := filepath.Join(dir, "plugin")
	pluginManifestDir := filepath.Join(pluginDir, ".claude-plugin")
	if err := os.MkdirAll(pluginManifestDir, 0o700); err != nil {
		return fmt.Errorf("create plugin dir: %w", err)
	}
	// Migrate away from the legacy layout: pre-fix runs wrote .mcp.json and
	// .claude-plugin/ at the agent root, which now collides with the plugin's
	// own .mcp.json (claude registers both, plugin-scope gets skipped).
	// Best-effort removal — ignore errors so an old layout can't block the
	// new one.
	_ = os.Remove(filepath.Join(dir, ".mcp.json"))
	_ = os.RemoveAll(filepath.Join(dir, ".claude-plugin"))

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve mesh binary path: %w", err)
	}

	// Single "mesh" MCP server entry for all channel types. The mesh-shim
	// connects to the daemon's router socket and multiplexes across platforms.
	settings := claudeSettings{
		MCPServers: map[string]mcpServerEntry{
			"mesh": {
				Command: exe,
				Args:    []string{"channel-server", "mesh-shim", "--agent", agentName},
			},
		},
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	// .mcp.json lives inside the plugin dir — NOT the agent cwd — so claude
	// only registers the mesh MCP server once (at plugin scope).
	mcpPath := filepath.Join(pluginDir, ".mcp.json")
	if err := os.WriteFile(mcpPath, data, 0o600); err != nil {
		return err
	}

	// Plugin scaffold: per-agent plugin manifest at <pluginDir>/.claude-plugin/plugin.json.
	if len(channels) > 0 {
		if err := writeAgentPluginManifest(agentName, channels); err != nil {
			return fmt.Errorf("write plugin manifest: %w", err)
		}
	}
	if err := SyncMeshMarketplace(); err != nil {
		// Marketplace is a convenience surface — failing here shouldn't break
		// agent start. Log and continue.
		fmt.Printf("mesh: marketplace sync failed: %v\n", err)
	}
	return nil
}


