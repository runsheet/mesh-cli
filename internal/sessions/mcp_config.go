package sessions

import (
	"encoding/json"
	"fmt"
	"os"

	"projectmesh.io/mesh-cli/internal/claudeconf"
	"projectmesh.io/mesh-cli/internal/paths"
)

// WriteSessionMCPConfig writes a per-session --mcp-config file at
// ~/.mesh/agents/<agentName>/.mesh-mcp.json and returns its absolute path.
//
// The file contains a single "mesh" mcpServers entry pointing at the current
// mesh binary with args ["channel-server", "mesh-shim", "--agent", agentName].
// Overwrites on every call so config drift (e.g. after a binary upgrade) is
// impossible.
func WriteSessionMCPConfig(agentName string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve mesh binary path: %w", err)
	}

	settings := claudeconf.Settings{
		MCPServers: map[string]claudeconf.MCPServerEntry{
			"mesh": {
				Command: exe,
				Args:    []string{"channel-server", "mesh-shim", "--agent", agentName},
			},
		},
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal mcp config: %w", err)
	}

	dir, err := paths.AgentWorkdir(agentName)
	if err != nil {
		return "", fmt.Errorf("agent workdir: %w", err)
	}

	path := dir + "/.mesh-mcp.json"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", fmt.Errorf("write mcp config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("rename mcp config: %w", err)
	}

	return path, nil
}
