package claudeconf

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// EnsureClaudeFolderTrusted pre-seeds ~/.claude.json so Claude Code doesn't
// prompt "Is this a project you trust?" on first spawn in the agent's working
// directory, AND registers the mesh MCP server in the project-scoped
// mcpServers map. The project-scoped registration is what makes
// `--dangerously-load-development-channels server:mesh` resolve — the
// `server:` form looks up MCP servers in ~/.claude.json (same registry
// `claude mcp add` writes to), not in --mcp-config.
//
// Best-effort: silently skips on any I/O or parse failure.
func EnsureClaudeFolderTrusted(agentWorkdir, agentName, exe string) {
	abs, err := filepath.Abs(agentWorkdir)
	if err != nil {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	cfgPath := filepath.Join(home, ".claude.json")

	meshEntry := map[string]any{
		"command": exe,
		"args":    []any{"channel-server", "mesh-shim", "--agent", agentName},
		"type":    "stdio",
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			minimal := map[string]any{
				"projects": map[string]any{
					abs: map[string]any{
						"hasTrustDialogAccepted": true,
						"mcpServers": map[string]any{
							"mesh": meshEntry,
						},
					},
				},
			}
			if b, err := json.MarshalIndent(minimal, "", "  "); err == nil {
				_ = atomicWriteFile(cfgPath, b, 0o600)
			}
		}
		return
	}

	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return
	}

	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		cfg["projects"] = projects
	}
	entry, _ := projects[abs].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
		projects[abs] = entry
	}

	changed := false
	if v, ok := entry["hasTrustDialogAccepted"].(bool); !ok || !v {
		entry["hasTrustDialogAccepted"] = true
		changed = true
	}

	servers, _ := entry["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		entry["mcpServers"] = servers
		changed = true
	}
	if !mcpEntryEqual(servers["mesh"], meshEntry) {
		servers["mesh"] = meshEntry
		changed = true
	}

	if !changed {
		return
	}

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWriteFile(cfgPath, b, 0o600)
}

func mcpEntryEqual(existing any, desired map[string]any) bool {
	em, ok := existing.(map[string]any)
	if !ok {
		return false
	}
	if em["command"] != desired["command"] {
		return false
	}
	if em["type"] != desired["type"] {
		return false
	}
	ea, _ := em["args"].([]any)
	da, _ := desired["args"].([]any)
	if len(ea) != len(da) {
		return false
	}
	for i := range ea {
		if ea[i] != da[i] {
			return false
		}
	}
	return true
}
