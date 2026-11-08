package nodev1

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// testMeshBinary builds a real mesh CLI binary once per test binary run and
// returns its path. A sidecar-mode lease's out-of-process egress execs the
// node's own binary (relayBinary); under go test that otherwise resolves to
// the compiled test binary, which does not understand "lease-egress".
var (
	meshBinaryOnce sync.Once
	meshBinaryPath string
	meshBinaryErr  error
	meshBinaryDir  string
)

func testMeshBinary(t *testing.T) string {
	t.Helper()
	meshBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mesh-test-binary")
		if err != nil {
			meshBinaryErr = err
			return
		}
		meshBinaryDir = dir
		bin := filepath.Join(dir, "mesh")
		build := exec.Command("go", "build", "-o", bin, "projectmesh.io/mesh-cli/cmd/mesh")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			meshBinaryErr = err
			t.Logf("building mesh: %v\n%s", err, out)
			return
		}
		if err := os.Chmod(bin, 0o755); err != nil {
			meshBinaryErr = err
			return
		}
		meshBinaryPath = bin
	})
	if meshBinaryErr != nil {
		t.Fatalf("building mesh for the out-of-process egress tests: %v", meshBinaryErr)
	}
	return meshBinaryPath
}

func cleanupTestMeshBinary() {
	if meshBinaryDir != "" {
		os.RemoveAll(meshBinaryDir)
	}
}
