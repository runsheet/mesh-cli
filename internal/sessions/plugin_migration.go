package sessions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"projectmesh.io/mesh-cli/internal/paths"
	"projectmesh.io/mesh-cli/internal/state"
)

// MigrateAwayFromPluginScaffold removes the legacy claude-code plugin/marketplace
// scaffold that predated per-session --mcp-config. Idempotent — safe to call on
// every daemon start. Each step is logged; errors are logged and skipped so a
// partial migration never aborts daemon startup.
func MigrateAwayFromPluginScaffold() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("mesh migrate-plugin: home dir: %v\n", err)
		return
	}

	// 1. Remove ~/.mesh/marketplace/
	mkDir := filepath.Join(home, ".mesh", "marketplace")
	if err := os.RemoveAll(mkDir); err != nil {
		fmt.Printf("mesh migrate-plugin: remove marketplace dir: %v\n", err)
	} else {
		fmt.Printf("mesh migrate-plugin: removed %s\n", mkDir)
	}

	// 2. Remove ~/.mesh/agents/<name>/plugin/ for each local agent.
	agents, err := state.ListAgents()
	if err != nil {
		fmt.Printf("mesh migrate-plugin: list agents: %v\n", err)
	} else {
		for _, a := range agents {
			dir, err := paths.AgentWorkdir(a.Name)
			if err != nil {
				fmt.Printf("mesh migrate-plugin: workdir %s: %v\n", a.Name, err)
				continue
			}
			pluginDir := filepath.Join(dir, "plugin")
			if err := os.RemoveAll(pluginDir); err != nil {
				fmt.Printf("mesh migrate-plugin: remove plugin dir %s: %v\n", pluginDir, err)
			} else {
				fmt.Printf("mesh migrate-plugin: removed %s\n", pluginDir)
			}
		}
	}

	// 3. Scrub @mesh entries from ~/.claude/plugins/installed_plugins.json.
	pluginsFile := filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
	if err := scrubMeshPluginEntries(pluginsFile); err != nil {
		fmt.Printf("mesh migrate-plugin: scrub installed_plugins.json: %v\n", err)
	}

	// 4. Remove ~/.claude/plugins/cache/mesh/
	cacheDir := filepath.Join(home, ".claude", "plugins", "cache", "mesh")
	if err := os.RemoveAll(cacheDir); err != nil {
		fmt.Printf("mesh migrate-plugin: remove cache dir: %v\n", err)
	} else {
		fmt.Printf("mesh migrate-plugin: removed %s\n", cacheDir)
	}
}

// scrubMeshPluginEntries removes keys ending with "@mesh" from the `plugins`
// map inside installed_plugins.json. The actual file shape is:
//
//	{ "version": 2, "plugins": { "larry-pa@mesh": [...], "github@claude-plugins-official": [...] } }
//
// No-ops if the file is absent, unparseable, or has no @mesh keys.
func scrubMeshPluginEntries(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}

	plugins, _ := doc["plugins"].(map[string]any)
	if plugins == nil {
		fmt.Printf("mesh migrate-plugin: no @mesh plugins to clean from installed_plugins.json\n")
		return nil
	}

	removed := 0
	for key := range plugins {
		if strings.HasSuffix(key, "@mesh") {
			delete(plugins, key)
			fmt.Printf("mesh migrate-plugin: removed installed_plugins.json entry %s\n", key)
			removed++
		}
	}

	if removed == 0 {
		fmt.Printf("mesh migrate-plugin: no @mesh plugins to clean from installed_plugins.json\n")
		return nil
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
