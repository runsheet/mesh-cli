package paths_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/paths"
)

func TestMeshDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got, err := paths.MeshDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(tmp, ".mesh")
	if got != want {
		t.Errorf("MeshDir() = %q, want %q", got, want)
	}
	// Should not create the directory.
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Error("MeshDir() must not create the directory")
	}
}

func TestEnsureMeshDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got, err := paths.EnsureMeshDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("EnsureMeshDir() did not create directory: %v", err)
	}
}

func TestAgentWorkdir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got, err := paths.AgentWorkdir("myagent")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(tmp, ".mesh", "agents", "myagent")
	if got != want {
		t.Errorf("AgentWorkdir() = %q, want %q", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("AgentWorkdir() did not create directory: %v", err)
	}
}

func TestDaemonSocketPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got, err := paths.DaemonSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "daemon.sock") {
		t.Errorf("DaemonSocketPath() = %q, want suffix daemon.sock", got)
	}
	if !strings.Contains(got, ".mesh") {
		t.Errorf("DaemonSocketPath() = %q, should contain .mesh", got)
	}
}

func TestRouterSockPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got, err := paths.RouterSockPath("myagent")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(tmp, ".mesh", "agents", "myagent", "mesh.sock")
	if got != want {
		t.Errorf("RouterSockPath() = %q, want %q", got, want)
	}
}

func TestRouterSubsFilePath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got, err := paths.RouterSubsFilePath("myagent")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(tmp, ".mesh", "agents", "myagent", "mesh-subs.json")
	if got != want {
		t.Errorf("RouterSubsFilePath() = %q, want %q", got, want)
	}
}
