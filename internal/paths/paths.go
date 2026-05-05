// Package paths centralises every os.UserHomeDir() callsite for the mesh
// daemon and CLI. All path helpers are pure functions of $HOME and their
// arguments; they do not create directories unless noted.
package paths

import (
	"os"
	"path/filepath"
)

// MeshDir returns the root of the mesh state directory (~/.mesh).
func MeshDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh"), nil
}

// EnsureMeshDir returns the mesh state directory, creating it if needed.
func EnsureMeshDir() (string, error) {
	dir, err := MeshDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// AgentWorkdir returns (and creates) the working directory for agentID:
// ~/.mesh/agents/<agentID>/
func AgentWorkdir(agentID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".mesh", "agents", agentID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// DaemonSocketPath returns the Unix socket path for the mesh daemon:
// ~/.mesh/daemon.sock
func DaemonSocketPath() (string, error) {
	dir, err := MeshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.sock"), nil
}

// RouterSockPath returns the per-agent router socket path:
// ~/.mesh/agents/<agentID>/mesh.sock
func RouterSockPath(agentID string) (string, error) {
	dir, err := AgentWorkdir(agentID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mesh.sock"), nil
}

// RouterSubsFilePath returns the path of the subscriptions file the router
// writes for the shim: ~/.mesh/agents/<agentID>/mesh-subs.json
func RouterSubsFilePath(agentID string) (string, error) {
	dir, err := AgentWorkdir(agentID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mesh-subs.json"), nil
}

// AgentChannelsDir returns ~/.mesh/agents/<agentID>/channels/ (not created).
func AgentChannelsDir(agentID string) (string, error) {
	dir, err := AgentWorkdir(agentID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "channels"), nil
}
