package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const localStateVersion = 1

// AgentDef is the durable definition of a locally-managed agent, persisted in
// ~/.mesh/state.json. env is intentionally not stored (secret hygiene — see
// spec §"Local state").
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
	// flag on every spawn of this agent. Autonomous loops set it via template;
	// `mesh run --dangerously-skip-permissions` sets it ad-hoc.
	DangerouslySkipPermissions bool    `json:"dangerouslySkipPermissions,omitempty"`
	CloudAgentID               *string `json:"cloudAgentId,omitempty"`
	// Channels materialize the template's channel list at `mesh run` time so
	// restart / autostart rebuild channel subprocesses without re-reading the
	// template. Opaque per-type config lives in ChannelSpec.Config.
	Channels []ChannelSpec `json:"channels,omitempty"`
}

type LocalState struct {
	Version int                  `json:"version"`
	Agents  map[string]*AgentDef `json:"agents"`
}

var (
	stateMu   sync.RWMutex
	statePath string
)

func meshDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh"), nil
}

func ensureMeshDir() (string, error) {
	dir, err := meshDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func stateFilePath() (string, error) {
	if statePath != "" {
		return statePath, nil
	}
	dir, err := meshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state.json"), nil
}

// LoadState returns the on-disk state or an empty state if the file doesn't
// exist yet. Callers must not assume the returned pointer is unique — wrap
// mutations in stateMu.
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
	if _, err := ensureMeshDir(); err != nil {
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

// UpdateAgent applies mutator under the state lock. The mutator may edit the
// passed *AgentDef; returns an error if the agent is missing.
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
