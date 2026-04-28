package claudeconf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/claudeconf"
)

func TestMigrateMCPFile_DiscordToMesh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")

	old := claudeconf.Settings{
		MCPServers: map[string]claudeconf.MCPServerEntry{
			"discord": {
				Command: "/usr/bin/mesh",
				Args:    []string{"channel-server", "discord-shim", "--agent", "larry-pa", "--index", "0", "--account", "larry"},
			},
		},
	}
	data, _ := json.MarshalIndent(old, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	claudeconf.MigrateMCPFile(path, "larry-pa", "/usr/bin/mesh", []string{"channel-server", "mesh-shim", "--agent", "larry-pa"})

	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got claudeconf.Settings
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatal(err)
	}
	if _, hasMesh := got.MCPServers["mesh"]; !hasMesh {
		t.Errorf("expected mcpServers.mesh after migration, got keys: %v", got.MCPServers)
	}
	if _, hasDiscord := got.MCPServers["discord"]; hasDiscord {
		t.Errorf("expected mcpServers.discord to be removed after migration")
	}
	if got.MCPServers["mesh"].Args[1] != "mesh-shim" {
		t.Errorf("expected args[1]=mesh-shim, got %v", got.MCPServers["mesh"].Args)
	}
}

func TestMigrateMCPFile_AlreadyMesh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")

	already := claudeconf.Settings{
		MCPServers: map[string]claudeconf.MCPServerEntry{
			"mesh": {
				Command: "/usr/bin/mesh",
				Args:    []string{"channel-server", "mesh-shim", "--agent", "larry-pa"},
			},
		},
	}
	data, _ := json.MarshalIndent(already, "", "  ")
	before := string(data)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	claudeconf.MigrateMCPFile(path, "larry-pa", "/usr/bin/mesh", []string{"channel-server", "mesh-shim", "--agent", "larry-pa"})

	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != before {
		t.Errorf("file should not change when already migrated;\nbefore: %s\nafter: %s", before, string(result))
	}
}

func TestMigrateMCPFile_Missing(t *testing.T) {
	// Should not panic or error when the file doesn't exist.
	claudeconf.MigrateMCPFile("/tmp/mesh-test-nonexistent-12345/.mcp.json", "agent", "/usr/bin/mesh", nil)
}
