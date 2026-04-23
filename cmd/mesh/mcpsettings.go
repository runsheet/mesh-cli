package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// envSource is retained for interface compatibility but is no longer used to
// hoist bot tokens — account credentials live in the layer-1 account dir and
// are read by the gateway, never passed through settings.json.
//
// When channels is empty we still write an empty settings.json — simpler than
// conditionally deleting, and claude tolerates it fine. This also keeps the
// file's presence stable across agent lifecycle, so `claude` picks it up on
// every spawn without us having to reason about ordering.
//
// The binary path is resolved via os.Executable so a `mesh` binary moved on
// disk after settings.json was written would invalidate the config — we
// re-write on every lifecycle event.
func WriteClaudeSettings(agentName string, channels []ChannelSpec, envSource map[string]string) error {
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

	// Count how many discord channels are declared so we can decide whether to
	// suffix the tool name.
	discordCount := 0
	for _, ch := range channels {
		if ch.Type == "discord" {
			discordCount++
		}
	}

	settings := claudeSettings{MCPServers: map[string]mcpServerEntry{}}
	for i, ch := range channels {
		key := fmt.Sprintf("mesh-channel-%s-%d", ch.Type, i)
		entry := mcpServerEntry{Command: exe}
		switch ch.Type {
		case "discord":
			args, env := discordMCPArgs(agentName, i, ch.Config, discordCount)
			entry.Args = args
			if len(env) > 0 {
				entry.Env = env
			}
		default:
			entry.Args = []string{"channel-server", ch.Type, "--agent", agentName, "--index", fmt.Sprintf("%d", i)}
		}
		settings.MCPServers[key] = entry
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(claudeDir, "settings.json")
	return os.WriteFile(path, data, 0o600)
}

// discordMCPArgs composes the `mesh channel-server discord-shim` CLI args
// from the template's per-channel config blob. The bot token lives in the
// account dir and is read by the gateway — it is never passed via env here.
//
// multiTool is true when the agent has more than one discord channel declared,
// in which case each shim's tool is suffixed by index to avoid collisions.
func discordMCPArgs(agentName string, index int, config map[string]any, totalDiscordCount int) ([]string, map[string]string) {
	account, _ := config["account"].(string)
	channelID, _ := config["channelId"].(string)

	args := []string{
		"channel-server", "discord-shim",
		"--agent", agentName,
		"--index", fmt.Sprintf("%d", index),
		"--account", account,
		"--channel-id", channelID,
	}

	if v, ok := config["inboundPrefix"].(string); ok && v != "" {
		args = append(args, "--inbound-prefix", v)
	}
	if users := joinSnowflakeList(config["allowedUsers"]); users != "" {
		args = append(args, "--allowed-users", users)
	}
	if roles := joinSnowflakeList(config["allowedRoles"]); roles != "" {
		args = append(args, "--allowed-roles", roles)
	}
	if req, _ := config["requireMention"].(bool); req {
		args = append(args, "--require-mention")
	}
	if totalDiscordCount > 1 {
		args = append(args, "--multi-tool")
	}

	return args, nil
}

// joinSnowflakeList turns the YAML/JSON representation of an ID list (may be
// []any of strings, []string, or a single comma-separated string) into a
// comma-separated string suitable for --allowed-users / --allowed-roles.
// Returns "" for empty input so callers can decide whether to emit the flag.
func joinSnowflakeList(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case []string:
		return strings.Join(x, ",")
	case []any:
		parts := make([]string, 0, len(x))
		for _, it := range x {
			if s, ok := it.(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	default:
		return ""
	}
}
