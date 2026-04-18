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

// WriteClaudeSettings materializes ~/.mesh/agents/<agentName>/.claude/settings.json
// with an mcpServers entry per configured channel. Keys are
// `mesh-channel-<type>-<idx>` so multiple channels of the same type coexist.
//
// When channels is empty we still write an empty settings.json — simpler than
// conditionally deleting, and claude tolerates it fine. This also keeps the
// file's presence stable across agent lifecycle, so `claude` picks it up on
// every spawn without us having to reason about ordering.
//
// The binary path is resolved via os.Executable so a `mesh` binary moved on
// disk after settings.json was written would invalidate the config — we
// re-write on every lifecycle event.
func WriteClaudeSettings(agentName string, channels []ChannelSpec) error {
	dir, err := agentWorkdir(agentName)
	if err != nil {
		return err
	}
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve mesh binary path: %w", err)
	}

	settings := claudeSettings{MCPServers: map[string]mcpServerEntry{}}
	for i, ch := range channels {
		key := fmt.Sprintf("mesh-channel-%s-%d", ch.Type, i)
		args := []string{"channel-server", ch.Type, "--agent", agentName, "--index", fmt.Sprintf("%d", i)}
		settings.MCPServers[key] = mcpServerEntry{
			Command: exe,
			Args:    args,
		}
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(claudeDir, "settings.json")
	return os.WriteFile(path, data, 0o600)
}
