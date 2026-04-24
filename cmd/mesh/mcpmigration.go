package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// migrateMCPSettingsToMesh rewrites .mcp.json files that still carry the old
// per-channel-type MCP server key (e.g. "discord") to the unified "mesh" key.
//
// Locations checked per agent:
//  1. ~/.mesh/agents/<name>/plugin/.mcp.json  (agent plugin dir)
//  2. ~/.claude/plugins/marketplaces/mesh/plugins/<name>/.mcp.json
//  3. ~/.claude/plugins/cache/mesh/<name>/<version>/.mcp.json  (installed
//     user-scope plugin cache — the copy Claude Code actually loads)
//
// The migration is idempotent: files that already have "mesh" as the only key
// (or that don't exist) are left untouched. Files with any key other than
// "mesh" are rewritten to a single "mesh" entry using the new args format.
func migrateMCPSettingsToMesh() {
	agents, err := ListAgents()
	if err != nil {
		fmt.Printf("mesh migrate-mcp: list agents: %v\n", err)
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("mesh migrate-mcp: home dir: %v\n", err)
		return
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("mesh migrate-mcp: executable: %v\n", err)
		return
	}

	for _, a := range agents {
		if len(a.Channels) == 0 {
			continue
		}
		newArgs := []string{"channel-server", "mesh-shim", "--agent", a.Name}
		candidates := []string{
			// Agent plugin dir (source of truth written by WriteClaudeSettings).
			filepath.Join(home, ".mesh", "agents", a.Name, "plugin", ".mcp.json"),
			// User-scope installed plugin (marketplace layout).
			filepath.Join(home, ".claude", "plugins", "marketplaces", "mesh", "plugins", a.Name, ".mcp.json"),
		}
		// Claude Code caches installed plugins at
		// ~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/.mcp.json.
		// Scan all version dirs for this agent.
		cacheBase := filepath.Join(home, ".claude", "plugins", "cache", "mesh", a.Name)
		if entries, err := os.ReadDir(cacheBase); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					candidates = append(candidates, filepath.Join(cacheBase, e.Name(), ".mcp.json"))
				}
			}
		}
		for _, path := range candidates {
			migrateMCPFile(path, a.Name, exe, newArgs)
		}
	}
}

// migrateMCPFile rewrites a single .mcp.json file in-place if it contains a
// non-"mesh" mcpServers entry. No-ops if the file doesn't exist, already has
// "mesh", or can't be parsed.
func migrateMCPFile(path, agentName, exe string, newArgs []string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // doesn't exist or unreadable — nothing to migrate
	}

	var settings claudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		fmt.Printf("mesh migrate-mcp: parse %s: %v\n", path, err)
		return
	}

	if settings.MCPServers == nil {
		return
	}

	// Already migrated.
	if _, hasMesh := settings.MCPServers["mesh"]; hasMesh && len(settings.MCPServers) == 1 {
		return
	}

	// Check if there's anything to migrate (any non-"mesh" key).
	hasNonMesh := false
	for k := range settings.MCPServers {
		if k != "mesh" {
			hasNonMesh = true
			break
		}
	}
	if !hasNonMesh {
		return
	}

	// Rebuild with just "mesh".
	settings.MCPServers = map[string]mcpServerEntry{
		"mesh": {
			Command: exe,
			Args:    newArgs,
		},
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fmt.Printf("mesh migrate-mcp: marshal %s: %v\n", path, err)
		return
	}
	if err := atomicWriteFile(path, out, 0o600); err != nil {
		fmt.Printf("mesh migrate-mcp: write %s: %v\n", path, err)
		return
	}
	fmt.Printf("mesh migrate-mcp: migrated %s (agent=%s) → mcpServers.mesh\n", path, agentName)
}
