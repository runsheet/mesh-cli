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
// envSource is the daemon's effective environment — used to hoist secret
// values (e.g. a bot token named in `tokenEnv`) onto the MCP server's `env`
// field so Claude passes them to the subprocess at spawn time. Keeping
// secrets out of the command line matters for `ps`/log hygiene.
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

	settings := claudeSettings{MCPServers: map[string]mcpServerEntry{}}
	for i, ch := range channels {
		key := fmt.Sprintf("mesh-channel-%s-%d", ch.Type, i)
		entry := mcpServerEntry{Command: exe}
		switch ch.Type {
		case "discord":
			args, env := discordMCPArgs(agentName, i, ch.Config, envSource)
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

// discordMCPArgs composes the `mesh channel-server discord` CLI args from the
// template's per-channel config blob. Secrets never go on the command line —
// the bot token is passed via the MCP server's `env` map instead (Claude
// forwards it to the subprocess at spawn time).
func discordMCPArgs(agentName string, index int, config map[string]any, envSource map[string]string) ([]string, map[string]string) {
	args := []string{
		"channel-server", "discord",
		"--agent", agentName,
		"--index", fmt.Sprintf("%d", index),
	}

	if v, ok := config["channelId"].(string); ok && v != "" {
		args = append(args, "--channel-id", v)
	}

	tokenEnv, _ := config["tokenEnv"].(string)
	if tokenEnv != "" {
		args = append(args, "--token-env", tokenEnv)
	}

	if v, ok := config["inboundPrefix"].(string); ok {
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

	// Hoist the bot token into the MCP server's env so it's available to the
	// subprocess at spawn time but never shows up in the command line, state
	// file, or logs. Missing values are tolerated — the channel-server will
	// emit a clear error on startup and exit.
	env := map[string]string{}
	if tokenEnv != "" {
		if v, ok := envSource[tokenEnv]; ok && v != "" {
			env[tokenEnv] = v
		}
	}
	return args, env
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
