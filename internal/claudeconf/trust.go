package claudeconf

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// EnsureClaudeFolderTrusted pre-seeds ~/.claude.json so Claude Code doesn't
// prompt "Is this a project you trust?" on first spawn in the agent's working
// directory, and also pre-approves the project-scoped .mcp.json servers so
// the second "Approve MCP servers for this project?" dialog is skipped.
//
// The function is best-effort: if the file is missing or malformed we skip
// silently.
func EnsureClaudeFolderTrusted(agentWorkdir string) {
	mcpServerNames := ReadMeshMCPServerNames(agentWorkdir)
	abs, err := filepath.Abs(agentWorkdir)
	if err != nil {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	cfgPath := filepath.Join(home, ".claude.json")

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			entry := map[string]any{"hasTrustDialogAccepted": true}
			if len(mcpServerNames) > 0 {
				entry["enabledMcpjsonServers"] = mcpServerNames
			}
			minimal := map[string]any{
				"projects": map[string]any{abs: entry},
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
	if len(mcpServerNames) > 0 {
		existing, _ := entry["enabledMcpjsonServers"].([]any)
		have := map[string]bool{}
		for _, s := range existing {
			if str, ok := s.(string); ok {
				have[str] = true
			}
		}
		merged := append([]any{}, existing...)
		mergedChanged := false
		for _, n := range mcpServerNames {
			if !have[n] {
				merged = append(merged, n)
				mergedChanged = true
			}
		}
		if mergedChanged {
			entry["enabledMcpjsonServers"] = merged
			changed = true
		}
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

// ReadMeshMCPServerNames parses <agentWorkdir>/.mcp.json and returns the
// keys under mcpServers. Best-effort — returns empty on any error.
func ReadMeshMCPServerNames(agentWorkdir string) []string {
	data, err := os.ReadFile(filepath.Join(agentWorkdir, ".mcp.json"))
	if err != nil {
		return nil
	}
	var wrap struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil
	}
	names := make([]string, 0, len(wrap.MCPServers))
	for k := range wrap.MCPServers {
		names = append(names, k)
	}
	return names
}
