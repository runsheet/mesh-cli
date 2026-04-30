// Package claudeconf manages the per-agent Claude Code settings files
// (.mcp.json and plugin manifests) used to configure the mesh MCP server.
package claudeconf

import (
	"encoding/json"
	"fmt"
	"os"

	"projectmesh.io/mesh-cli/internal/paths"
	"projectmesh.io/mesh-cli/internal/state"
)

// MCPServerEntry is the shape Claude Code expects under mcpServers in
// .claude/settings.json: a command + args list that it spawns with stdio
// piped for JSON-RPC.
type MCPServerEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Settings is the shape of the .mcp.json file Claude Code reads.
type Settings struct {
	MCPServers map[string]MCPServerEntry `json:"mcpServers,omitempty"`
}

// MigrateMCPSettingsToMesh rewrites .mcp.json files that still carry the old
// per-channel-type MCP server key (e.g. "discord") to the unified "mesh" key.
// The migration is idempotent.
func MigrateMCPSettingsToMesh(exe string) {
	agents, err := state.ListAgents()
	if err != nil {
		fmt.Printf("mesh migrate-mcp: list agents: %v\n", err)
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("mesh migrate-mcp: home dir: %v\n", err)
		return
	}

	for _, a := range agents {
		if len(a.Channels) == 0 {
			continue
		}
		newArgs := []string{"channel-server", "mesh-shim", "--agent", a.Name}
		candidates := []string{
			home + "/.mesh/agents/" + a.Name + "/plugin/.mcp.json",
			home + "/.claude/plugins/marketplaces/mesh/plugins/" + a.Name + "/.mcp.json",
		}
		cacheBase := home + "/.claude/plugins/cache/mesh/" + a.Name
		if entries, err := os.ReadDir(cacheBase); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					candidates = append(candidates, cacheBase+"/"+e.Name()+"/.mcp.json")
				}
			}
		}
		for _, path := range candidates {
			MigrateMCPFile(path, a.Name, exe, newArgs)
		}
	}
}

// MigrateMCPFile rewrites a single .mcp.json file in-place if it contains a
// non-"mesh" mcpServers entry. No-ops if the file doesn't exist, already has
// "mesh", or can't be parsed.
func MigrateMCPFile(path, agentName, exe string, newArgs []string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var settings Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		fmt.Printf("mesh migrate-mcp: parse %s: %v\n", path, err)
		return
	}

	if settings.MCPServers == nil {
		return
	}

	if _, hasMesh := settings.MCPServers["mesh"]; hasMesh && len(settings.MCPServers) == 1 {
		return
	}

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

	settings.MCPServers = map[string]MCPServerEntry{
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

// AgentMCPPath returns the canonical per-session MCP config path for the agent.
// This is the file written by sessions.WriteSessionMCPConfig and passed to
// claude via --mcp-config on every spawn.
func AgentMCPPath(agentName string) (string, error) {
	dir, err := paths.AgentWorkdir(agentName)
	if err != nil {
		return "", err
	}
	return dir + "/.mesh-mcp.json", nil
}

// atomicWriteFile writes data to path via tmp+rename so a concurrent reader
// never sees a partial write.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
