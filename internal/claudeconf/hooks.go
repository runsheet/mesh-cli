package claudeconf

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// hookCommand is the shape of one entry in a hook handler list.
type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// hookMatcher is one element in an event's hook list.
type hookMatcher struct {
	Hooks []hookCommand `json:"hooks"`
}

// globalSettings is a partial unmarshal of ~/.claude/settings.json covering
// only the fields we read or write. Unknown fields are preserved via RawMessage.
type globalSettings struct {
	raw   map[string]json.RawMessage
	Hooks map[string][]hookMatcher
}

// readGlobalSettings reads ~/.claude/settings.json into a map so that fields
// we don't own are preserved on write.
func readGlobalSettings() (map[string]json.RawMessage, error) {
	path, err := globalSettingsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse settings.json: %w", err)
	}
	return m, nil
}

func globalSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// InstallPokeHook idempotently writes the mesh-poke hook entry into
// ~/.claude/settings.json. scriptPath is the absolute path of the installed
// hook script. If the hook is already registered with the same command, the
// file is not rewritten.
func InstallPokeHook(scriptPath string) error {
	settingsPath, err := globalSettingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		return err
	}

	raw, err := readGlobalSettings()
	if err != nil {
		return err
	}

	// Decode the existing hooks block if present.
	type hookEntry struct {
		Hooks []hookCommand `json:"hooks"`
	}
	existing := map[string][]hookEntry{}
	if v, ok := raw["hooks"]; ok {
		_ = json.Unmarshal(v, &existing)
	}

	// Check if our hook is already registered for the Stop event.
	for _, entry := range existing["Stop"] {
		for _, h := range entry.Hooks {
			if h.Type == "command" && h.Command == scriptPath {
				return nil // already installed, nothing to do
			}
		}
	}

	// Append our hook.
	existing["Stop"] = append(existing["Stop"], hookEntry{
		Hooks: []hookCommand{{
			Type:    "command",
			Command: scriptPath,
			Timeout: 5,
		}},
	})

	hooksJSON, err := json.Marshal(existing)
	if err != nil {
		return err
	}
	raw["hooks"] = hooksJSON

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(settingsPath, out, 0o600)
}

// InstallPokeScript writes the poke hook shell script to
// ~/.mesh/hooks/mesh-poke.sh and returns its absolute path.
// scriptContent is the raw shell script text (from the API or embedded).
// The file is only rewritten when the content differs.
func InstallPokeScript(scriptContent string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".mesh", "hooks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "mesh-poke.sh")

	existing, readErr := os.ReadFile(path)
	if readErr == nil && string(existing) == scriptContent {
		return path, nil // unchanged
	}

	if err := atomicWriteFile(path, []byte(scriptContent), 0o700); err != nil {
		return "", err
	}
	return path, nil
}
