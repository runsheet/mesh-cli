package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseTargetURL(t *testing.T) {
	cases := []struct {
		input    string
		platform string
		subtype  string
		id       string
		wantErr  bool
	}{
		{"discord://channel/123456789", "discord", "channel", "123456789", false},
		{"discord://dm/987654321", "discord", "dm", "987654321", false},
		{"discord://channel/123/extra", "discord", "channel", "123/extra", false}, // extra path kept in id
		{"nodomain", "", "", "", true},
		{"discord://channel/", "", "", "", true},
		{"discord://noid", "", "", "", true},
	}

	for _, tc := range cases {
		platform, subtype, id, errMsg := parseTargetURL(tc.input)
		if tc.wantErr {
			if errMsg == "" {
				t.Errorf("parseTargetURL(%q): expected error, got platform=%q subtype=%q id=%q", tc.input, platform, subtype, id)
			}
			continue
		}
		if errMsg != "" {
			t.Errorf("parseTargetURL(%q): unexpected error: %s", tc.input, errMsg)
			continue
		}
		if platform != tc.platform {
			t.Errorf("parseTargetURL(%q) platform: got %q want %q", tc.input, platform, tc.platform)
		}
		if subtype != tc.subtype {
			t.Errorf("parseTargetURL(%q) subtype: got %q want %q", tc.input, subtype, tc.subtype)
		}
		if id != tc.id {
			t.Errorf("parseTargetURL(%q) id: got %q want %q", tc.input, id, tc.id)
		}
	}
}

func TestMigrateMCPFile_DiscordToMesh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")

	// Write an old-style discord key.
	old := claudeSettings{
		MCPServers: map[string]mcpServerEntry{
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

	migrateMCPFile(path, "larry-pa", "/usr/bin/mesh", []string{"channel-server", "mesh-shim", "--agent", "larry-pa"})

	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got claudeSettings
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

	already := claudeSettings{
		MCPServers: map[string]mcpServerEntry{
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

	migrateMCPFile(path, "larry-pa", "/usr/bin/mesh", []string{"channel-server", "mesh-shim", "--agent", "larry-pa"})

	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// File should be unchanged.
	if string(result) != before {
		t.Errorf("file should not change when already migrated;\nbefore: %s\nafter: %s", before, string(result))
	}
}

func TestMigrateMCPFile_Missing(t *testing.T) {
	// Should not panic or error when the file doesn't exist.
	migrateMCPFile("/tmp/mesh-test-nonexistent-12345/.mcp.json", "agent", "/usr/bin/mesh", nil)
}

func TestMeshLastInboundReplyTarget(t *testing.T) {
	l := &meshLastInbound{}

	// No inbound yet.
	source, target := l.replyTarget()
	if source != "" || target != "" {
		t.Errorf("expected empty source+target before any inbound, got source=%q target=%q", source, target)
	}

	// Discord channel inbound.
	l.set("discord", "123456789")
	source, target = l.replyTarget()
	if source != "discord" {
		t.Errorf("source: got %q want discord", source)
	}
	if target != "discord://channel/123456789" {
		t.Errorf("target: got %q want discord://channel/123456789", target)
	}
}
