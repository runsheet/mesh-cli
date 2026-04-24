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

// discordShimConfigFile is the per-shim config written to
// ~/.mesh/agents/<name>/channels/discord-<idx>.config.json.
// The shim reads this at startup instead of receiving everything on the CLI,
// which avoids building a JSON-in-flag escape mess for the overrides list.
type discordShimConfigFile struct {
	SubscriptionMode string                  `json:"subscriptionMode"`
	Account          string                  `json:"account"`
	ChannelID        string                  `json:"channelId,omitempty"`
	GuildID          string                  `json:"guildId,omitempty"`
	RequireMention   bool                    `json:"requireMention"`
	AllowedUsers     []string                `json:"allowedUsers,omitempty"`
	AllowedRoles     []string                `json:"allowedRoles,omitempty"`
	InboundPrefix    string                  `json:"inboundPrefix,omitempty"`
	MultiTool        bool                    `json:"multiTool"`
	ChannelOverrides []discordChannelOverride `json:"channelOverrides,omitempty"`
}

// discordShimConfigFilePath returns the path of the per-shim config file.
func discordShimConfigFilePath(agentName string, index int) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".mesh", "agents", agentName, "channels")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create channels dir: %w", err)
	}
	return filepath.Join(dir, fmt.Sprintf("discord-%d.config.json", index)), nil
}

// writeDiscordShimConfig writes the per-shim config file and returns its path.
func writeDiscordShimConfig(agentName string, index int, cfg discordShimConfigFile) (string, error) {
	path, err := discordShimConfigFilePath(agentName, index)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal shim config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", fmt.Errorf("write shim config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("rename shim config: %w", err)
	}
	return path, nil
}

// WriteClaudeSettings materializes the per-agent plugin under
// ~/.mesh/agents/<agentName>/plugin/. Claude Code is spawned with
// --plugin-dir pointing at that subdir, so the MCP servers register at
// plugin scope (which is the scope where experimental.claude/channel is
// honored). Crucially, no .mcp.json is written to the agent cwd —
// project-scope and plugin-scope same-keyed servers conflict, with
// project-scope winning and plugin-scope getting silently skipped.
//
// For discord channels it also writes a per-shim config file under
// ~/.mesh/agents/<agentName>/channels/discord-<idx>.config.json so the shim
// can pick up the full configuration (including channelOverrides) without
// requiring JSON-escaped CLI flags.
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

	// Count how many discord channels are declared so we can decide whether to
	// suffix the tool name.
	discordCount := 0
	for _, ch := range channels {
		if ch.Type == "discord" {
			discordCount++
		}
	}

	settings := claudeSettings{MCPServers: map[string]mcpServerEntry{}}
	// Track per-type counts so single-channel-of-type gets the bare type name
	// (e.g. "discord" — matching the official plugin's MCP key, which Claude
	// Code may gate channel-mode features on) and multi-channel agents get
	// indexed keys.
	typeCount := map[string]int{}
	for _, ch := range channels {
		typeCount[ch.Type]++
	}
	typeIdx := map[string]int{}
	for i, ch := range channels {
		var key string
		if typeCount[ch.Type] == 1 {
			key = ch.Type
		} else {
			key = fmt.Sprintf("%s-%d", ch.Type, typeIdx[ch.Type])
			typeIdx[ch.Type]++
		}
		entry := mcpServerEntry{Command: exe}
		switch ch.Type {
		case "discord":
			args, err := discordMCPArgs(agentName, i, ch.Config, discordCount)
			if err != nil {
				return fmt.Errorf("channels[%d]: write shim config: %w", i, err)
			}
			entry.Args = args
		default:
			entry.Args = []string{"channel-server", ch.Type, "--agent", agentName, "--index", fmt.Sprintf("%d", i)}
		}
		settings.MCPServers[key] = entry
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	// .mcp.json lives inside the plugin dir — NOT the agent cwd — so claude
	// only registers the discord MCP server once (at plugin scope).
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

// discordMCPArgs writes the per-shim config file and returns the slim
// `mesh channel-server discord-shim` CLI args that reference it.
// The bot token lives in the account dir and is read by the gateway — it is
// never passed via env here.
func discordMCPArgs(agentName string, index int, config map[string]any, totalDiscordCount int) ([]string, error) {
	dcc := parseDiscordChannelConfig(config)

	shimCfg := discordShimConfigFile{
		SubscriptionMode: dcc.SubscriptionMode,
		Account:          dcc.Account,
		ChannelID:        dcc.ChannelID,
		GuildID:          dcc.GuildID,
		RequireMention:   dcc.RequireMention,
		AllowedUsers:     dcc.AllowedUsers,
		AllowedRoles:     dcc.AllowedRoles,
		InboundPrefix:    dcc.InboundPrefix,
		MultiTool:        totalDiscordCount > 1,
		ChannelOverrides: dcc.ChannelOverrides,
	}

	if _, err := writeDiscordShimConfig(agentName, index, shimCfg); err != nil {
		return nil, err
	}

	args := []string{
		"channel-server", "discord-shim",
		"--agent", agentName,
		"--index", fmt.Sprintf("%d", index),
		"--account", dcc.Account,
	}
	return args, nil
}

