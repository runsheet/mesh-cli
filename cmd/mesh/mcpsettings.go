package main

// This file is a thin compatibility shim. MCP settings types now live in
// internal/claudeconf; the aliases below let cmd/mesh/ continue to use the
// old names without change until Phase 9.
//
// TODO(spec22-phase9): inline all call sites to internal/claudeconf.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"projectmesh.io/mesh-cli/internal/claudeconf"
)

type mcpServerEntry = claudeconf.MCPServerEntry
type claudeSettings = claudeconf.Settings

// WriteClaudeSettings stays here because it depends on writeAgentPluginManifest
// and SyncMeshMarketplace from pluginscaffold.go, which haven't migrated yet.
// TODO(spec22-phase9): move to internal/claudeconf after pluginscaffold migrates.
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
	_ = os.Remove(filepath.Join(dir, ".mcp.json"))
	_ = os.RemoveAll(filepath.Join(dir, ".claude-plugin"))

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve mesh binary path: %w", err)
	}

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
	mcpPath := filepath.Join(pluginDir, ".mcp.json")
	if err := os.WriteFile(mcpPath, data, 0o600); err != nil {
		return err
	}

	if len(channels) > 0 {
		if err := writeAgentPluginManifest(agentName, channels); err != nil {
			return fmt.Errorf("write plugin manifest: %w", err)
		}
	}
	if err := SyncMeshMarketplace(); err != nil {
		fmt.Printf("mesh: marketplace sync failed: %v\n", err)
	}
	return nil
}
