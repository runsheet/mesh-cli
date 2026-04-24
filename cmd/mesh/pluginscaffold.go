package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Mesh maintains a single Claude Code plugin marketplace at
// ~/.mesh/marketplace/. Each agent that has channels becomes one plugin in
// that marketplace. The user registers the marketplace once with Claude Code:
//
//	claude /plugin marketplace add ~/.mesh/marketplace
//
// …and then installs per-agent plugins as needed:
//
//	claude /plugin install larry-pa@mesh
//
// Why a plugin instead of just .mcp.json? Claude Code only honors the
// experimental.claude/channel capability (the mechanism that turns
// notifications/claude/channel into user turns) for plugin-scoped MCP
// servers. Project-scoped .mcp.json servers are loaded but their
// experimental capabilities are gated.
//
// Each per-agent plugin.json + .mcp.json lives inside the agent's working
// directory at ~/.mesh/agents/<name>/.claude-plugin/plugin.json so the
// existing settings-write path can keep them in sync. The marketplace
// references the agent dirs via relative paths, so changes to an agent's
// channel config flow through without re-registering.

const meshMarketplaceName = "mesh"

// pluginManifest is the shape of <agent-dir>/.claude-plugin/plugin.json.
// Mirrors the official discord plugin manifest at
// ~/.claude/plugins/marketplaces/claude-plugins-official/external_plugins/discord/.claude-plugin/plugin.json
type pluginManifest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Version     string   `json:"version"`
	Keywords    []string `json:"keywords,omitempty"`
}

// marketplaceManifest is the shape of
// ~/.mesh/marketplace/.claude-plugin/marketplace.json.
type marketplaceManifest struct {
	Schema      string                 `json:"$schema,omitempty"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Owner       map[string]string      `json:"owner,omitempty"`
	Plugins     []marketplacePluginRef `json:"plugins"`
}

type marketplacePluginRef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
	Category    string `json:"category,omitempty"`
}

// MeshMarketplaceDir returns the path to the mesh-managed marketplace.
func MeshMarketplaceDir() (string, error) {
	dir, err := meshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "marketplace"), nil
}

// writeAgentPluginManifest writes <agent-dir>/.claude-plugin/plugin.json so
// the agent's working directory can be loaded by Claude Code as a plugin.
// Called from WriteClaudeSettings whenever an agent has at least one channel.
func writeAgentPluginManifest(agentName string, channels []ChannelSpec) error {
	dir, err := agentWorkdir(agentName)
	if err != nil {
		return err
	}
	pluginDir := filepath.Join(dir, ".claude-plugin")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		return err
	}
	manifest := pluginManifest{
		Name:        agentName,
		Description: agentPluginDescription(agentName, channels),
		Version:     "0.1.0",
		Keywords:    []string{"mesh", "channel"},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(pluginDir, "plugin.json"), data, 0o600)
}

// agentPluginDescription builds a short, channel-aware description that
// shows up in `claude /plugin list` output.
func agentPluginDescription(agentName string, channels []ChannelSpec) string {
	types := map[string]bool{}
	for _, ch := range channels {
		types[ch.Type] = true
	}
	if len(types) == 0 {
		return fmt.Sprintf("Mesh agent %q (no channels configured)", agentName)
	}
	tlist := make([]string, 0, len(types))
	for t := range types {
		tlist = append(tlist, t)
	}
	sort.Strings(tlist)
	return fmt.Sprintf("Mesh agent %q — channels: %s", agentName, strings.Join(tlist, ", "))
}

// SyncMeshMarketplace rebuilds ~/.mesh/marketplace/.claude-plugin/marketplace.json
// from the current set of registered agents that have channels. Idempotent;
// safe to call on every settings-write event.
func SyncMeshMarketplace() error {
	mkDir, err := MeshMarketplaceDir()
	if err != nil {
		return err
	}
	pluginDir := filepath.Join(mkDir, ".claude-plugin")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		return err
	}

	agents, err := ListAgents()
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	plugins := make([]marketplacePluginRef, 0, len(agents))
	for _, a := range agents {
		if len(a.Channels) == 0 {
			continue
		}
		// Source is a path relative to the marketplace dir. Marketplace lives
		// at ~/.mesh/marketplace; agents live at ~/.mesh/agents/<name>; so the
		// relative path is "../agents/<name>".
		agentDir := filepath.Join(homeDir, ".mesh", "agents", a.Name)
		rel, err := filepath.Rel(mkDir, agentDir)
		if err != nil {
			rel = agentDir
		}
		plugins = append(plugins, marketplacePluginRef{
			Name:        a.Name,
			Description: agentPluginDescription(a.Name, a.Channels),
			Source:      "./" + rel,
			Category:    "mesh",
		})
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Name < plugins[j].Name })

	manifest := marketplaceManifest{
		Schema:      "https://anthropic.com/claude-code/marketplace.schema.json",
		Name:        meshMarketplaceName,
		Description: "Locally-managed marketplace generated by the mesh CLI. One plugin per registered mesh agent that has channels.",
		Owner: map[string]string{
			"name": "mesh",
		},
		Plugins: plugins,
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(pluginDir, "marketplace.json"), data, 0o600)
}
