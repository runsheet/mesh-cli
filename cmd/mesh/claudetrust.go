package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ensureClaudeFolderTrusted pre-seeds ~/.claude.json so Claude Code doesn't
// prompt "Is this a project you trust?" on first spawn in the agent's working
// directory.
//
// Claude Code stores trust state in a top-level `projects` map keyed by the
// absolute directory path; setting `hasTrustDialogAccepted: true` there skips
// the interactive prompt. Without this, agents spawned by the daemon hang on
// the trust dialog because stdin is piped to the MCP channel, not a human.
//
// The function is best-effort: if the file is missing or malformed we skip
// silently — Claude will fall back to the prompt, which is no worse than the
// pre-existing behavior. We never clobber fields other than
// `hasTrustDialogAccepted`.
func ensureClaudeFolderTrusted(agentWorkdir string) {
	abs, err := filepath.Abs(agentWorkdir)
	if err != nil {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	cfgPath := filepath.Join(home, ".claude.json")

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		// File may not exist yet if the user has never run claude locally.
		// Create a minimal one with the trust entry so we don't hang on first
		// spawn.
		if os.IsNotExist(err) {
			minimal := map[string]any{
				"projects": map[string]any{
					abs: map[string]any{"hasTrustDialogAccepted": true},
				},
			}
			if b, err := json.MarshalIndent(minimal, "", "  "); err == nil {
				_ = atomicWriteFile(cfgPath, b, 0o600)
			}
		}
		return
	}

	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return
	}

	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		cfg["projects"] = projects
	}
	entry, _ := projects[abs].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
		projects[abs] = entry
	}
	// Short-circuit if already trusted — avoids needless writes to a large
	// config file that the real claude process may also be updating.
	if v, ok := entry["hasTrustDialogAccepted"].(bool); ok && v {
		return
	}
	entry["hasTrustDialogAccepted"] = true

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWriteFile(cfgPath, b, 0o600)
}

// atomicWriteFile writes data to path via tmp+rename so a concurrent reader
// never sees a partial file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
