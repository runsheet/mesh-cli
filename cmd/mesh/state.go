package main

// This file is a thin compatibility shim. All state types and helpers now live
// in internal/state; the aliases and wrappers below let the rest of cmd/mesh/
// continue to use the old names without change until Phase 8 rewires callers
// directly to internal/state.
//
// TODO(spec22-phase8): inline all call sites to internal/state.

import (
	"path/filepath"

	"projectmesh.io/mesh-cli/internal/paths"
	"projectmesh.io/mesh-cli/internal/state"
)

const localStateVersion = 1

type AgentDef = state.AgentDef
type LocalState = state.LocalState

// statePath is the test-injection override, kept for test compatibility.
// Tests in cmd/mesh that set statePath still work; internal/state uses
// state.SetStatePath instead.
var statePath string

func stateFilePath() (string, error) {
	if statePath != "" {
		state.SetStatePath(statePath)
		return statePath, nil
	}
	state.SetStatePath("")
	dir, err := paths.MeshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state.json"), nil
}

func meshDir() (string, error)       { return paths.MeshDir() }
func ensureMeshDir() (string, error) { return paths.EnsureMeshDir() }

func LoadState() (*LocalState, error)           { return state.LoadState() }
func SaveState(s *LocalState) error             { return state.SaveState(s) }
func AddAgent(def *AgentDef) error              { return state.AddAgent(def) }
func RemoveAgent(name string) (bool, error)     { return state.RemoveAgent(name) }
func GetAgent(name string) (*AgentDef, bool, error) { return state.GetAgent(name) }
func ListAgents() ([]*AgentDef, error)          { return state.ListAgents() }
func UpdateAgent(name string, mutate func(*AgentDef)) error {
	return state.UpdateAgent(name, mutate)
}
