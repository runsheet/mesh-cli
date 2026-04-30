// Package state manages the durable local agent registry persisted in
// ~/.mesh/state.json. All exported functions are safe for concurrent use.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/channels"
	"projectmesh.io/mesh-cli/internal/paths"
)

const localStateVersion = 1

// AgentDef is the durable definition of a locally-managed agent, persisted in
// ~/.mesh/state.json. env is intentionally not stored (secret hygiene).
type AgentDef struct {
	Name              string    `json:"name"`
	TemplateSource    string    `json:"templateSource"`
	TemplateRef       string    `json:"templateRef,omitempty"`
	TemplateSha       string    `json:"templateSha,omitempty"`
	TemplateCachePath string    `json:"templateCachePath,omitempty"`
	Model             string    `json:"model,omitempty"`
	// RequiredEnv lists the names (not values) of env vars the template
	// declared as required at the last successful run. Stored for inspect UX;
	// the secrets themselves are never persisted.
	RequiredEnv     []string  `json:"requiredEnv,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	LastSessionID   string    `json:"lastSessionId,omitempty"`
	// ClaudeSessionID is the UUID we pass to `claude --session-id` on every
	// spawn of this agent. Stable across daemon restarts, so `mesh restart`
	// and auto-start-on-boot resume the same conversation. Regenerated on
	// explicit `new-up` to drop the prior context.
	ClaudeSessionID string `json:"claudeSessionId,omitempty"`
	// DangerouslySkipPermissions surfaces claude's --dangerously-skip-permissions
	// flag on every spawn of this agent.
	DangerouslySkipPermissions bool    `json:"dangerouslySkipPermissions,omitempty"`
	CloudAgentID               *string `json:"cloudAgentId,omitempty"`
	// Description is the template's spec.description field, persisted so the
	// peers broker can auto-populate the Summary on registration without
	// re-reading the snapshot.
	Description string `json:"description,omitempty"`
	// Role is the template's spec.role field, propagated to mesh-api on adopt/sync
	// so the agent can be auto-assigned to the matching project role slot.
	Role string `json:"role,omitempty"`
	// Channels materialize the template's channel list at `mesh run` time so
	// restart / autostart rebuild channel subprocesses without re-reading the
	// template.
	Channels []channels.Spec `json:"channels,omitempty"`
	// Values is the merged values map (template defaults + --values + --set)
	// that produced this agent's rendered agent.yaml / system_prompt.
	Values map[string]any `json:"values,omitempty"`
	// TemplateValues holds the raw --values file paths supplied at run time.
	TemplateValues []string `json:"templateValues,omitempty"`
	// TemplateOverrides holds the --set / --set-string key=value pairs supplied
	// at run time, encoded as "key=value" strings.
	TemplateOverrides []string `json:"templateOverrides,omitempty"`
}

// LocalState is the top-level JSON envelope in ~/.mesh/state.json.
type LocalState struct {
	Version int                 `json:"version"`
	Agents  map[string]*AgentDef `json:"agents"`
}

var (
	stateMu   sync.RWMutex
	// statePath is the override path used by tests. Set via SetStatePath.
	statePath string
)

// SetStatePath overrides the default ~/.mesh/state.json path. Intended for
// tests that need an isolated state file.
func SetStatePath(path string) {
	stateMu.Lock()
	defer stateMu.Unlock()
	statePath = path
}

func stateFilePath() (string, error) {
	if statePath != "" {
		return statePath, nil
	}
	dir, err := paths.MeshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state.json"), nil
}

// LoadState returns the on-disk state or an empty state if the file doesn't
// exist yet. Callers must not assume the returned pointer is unique — wrap
// mutations in the caller's own locking or use the mutating helpers below.
func LoadState() (*LocalState, error) {
	path, err := stateFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &LocalState{Version: localStateVersion, Agents: map[string]*AgentDef{}}, nil
		}
		return nil, err
	}
	var s LocalState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state.json: %w", err)
	}
	if s.Agents == nil {
		s.Agents = map[string]*AgentDef{}
	}
	if s.Version == 0 {
		s.Version = localStateVersion
	}
	return &s, nil
}

// SaveState writes via temp-file + rename so we don't corrupt on crash.
func SaveState(s *LocalState) error {
	if _, err := paths.EnsureMeshDir(); err != nil {
		return err
	}
	path, err := stateFilePath()
	if err != nil {
		return err
	}
	s.Version = localStateVersion
	if s.Agents == nil {
		s.Agents = map[string]*AgentDef{}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AddAgent adds def to state.json, failing if the name already exists.
func AddAgent(def *AgentDef) error {
	if def == nil || def.Name == "" {
		return errors.New("agent def requires name")
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	s, err := LoadState()
	if err != nil {
		return err
	}
	if _, exists := s.Agents[def.Name]; exists {
		return fmt.Errorf("agent %q already exists", def.Name)
	}
	if def.CreatedAt.IsZero() {
		def.CreatedAt = time.Now().UTC()
	}
	s.Agents[def.Name] = def
	return SaveState(s)
}

// RemoveAgent deletes an agent from state.json. Returns (false, nil) if not
// found.
func RemoveAgent(name string) (bool, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	s, err := LoadState()
	if err != nil {
		return false, err
	}
	if _, ok := s.Agents[name]; !ok {
		return false, nil
	}
	delete(s.Agents, name)
	return true, SaveState(s)
}

// GetAgent returns the agent definition for name, or (nil, false, nil) if not
// found.
func GetAgent(name string) (*AgentDef, bool, error) {
	stateMu.RLock()
	defer stateMu.RUnlock()
	s, err := LoadState()
	if err != nil {
		return nil, false, err
	}
	def, ok := s.Agents[name]
	return def, ok, nil
}

// ListAgents returns all agents sorted by name.
func ListAgents() ([]*AgentDef, error) {
	stateMu.RLock()
	defer stateMu.RUnlock()
	s, err := LoadState()
	if err != nil {
		return nil, err
	}
	out := make([]*AgentDef, 0, len(s.Agents))
	for _, a := range s.Agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// UpdateAgent applies mutator under the state lock. Returns an error if the
// agent is missing.
func UpdateAgent(name string, mutate func(*AgentDef)) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	s, err := LoadState()
	if err != nil {
		return err
	}
	def, ok := s.Agents[name]
	if !ok {
		return fmt.Errorf("agent %q not found", name)
	}
	mutate(def)
	return SaveState(s)
}
