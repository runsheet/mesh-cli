package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestScrubMeshPluginEntries(t *testing.T) {
	t.Run("removes @mesh keys and preserves others", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "installed_plugins.json")

		fixture := map[string]any{
			"version": 2,
			"plugins": map[string]any{
				"github@claude-plugins-official": []any{map[string]any{"id": "gh"}},
				"larry-pa@mesh":                 []any{map[string]any{"id": "larry"}},
				"researcher@mesh":               []any{map[string]any{"id": "res"}},
			},
		}
		data, _ := json.MarshalIndent(fixture, "", "  ")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}

		if err := scrubMeshPluginEntries(path); err != nil {
			t.Fatalf("scrubMeshPluginEntries: %v", err)
		}

		result, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(result, &doc); err != nil {
			t.Fatalf("parse result: %v", err)
		}

		plugins, _ := doc["plugins"].(map[string]any)
		if plugins == nil {
			t.Fatal("plugins key missing from result")
		}
		if _, ok := plugins["larry-pa@mesh"]; ok {
			t.Error("larry-pa@mesh should have been removed")
		}
		if _, ok := plugins["researcher@mesh"]; ok {
			t.Error("researcher@mesh should have been removed")
		}
		if _, ok := plugins["github@claude-plugins-official"]; !ok {
			t.Error("github@claude-plugins-official should have been preserved")
		}
		if v, _ := doc["version"].(float64); v != 2 {
			t.Errorf("version field should be preserved as 2, got %v", doc["version"])
		}
	})

	t.Run("no-op when no @mesh keys present", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "installed_plugins.json")

		fixture := map[string]any{
			"version": 2,
			"plugins": map[string]any{
				"github@claude-plugins-official": []any{},
			},
		}
		data, _ := json.MarshalIndent(fixture, "", "  ")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}

		stat1, _ := os.Stat(path)
		if err := scrubMeshPluginEntries(path); err != nil {
			t.Fatalf("scrubMeshPluginEntries: %v", err)
		}
		stat2, _ := os.Stat(path)

		// mtime should be unchanged (no write occurred)
		if !stat1.ModTime().Equal(stat2.ModTime()) {
			t.Error("file was rewritten despite no @mesh keys — mtime changed unnecessarily")
		}
	})

	t.Run("no-op when file is absent", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "missing.json")
		if err := scrubMeshPluginEntries(path); err != nil {
			t.Fatalf("expected nil for missing file, got: %v", err)
		}
	})
}
