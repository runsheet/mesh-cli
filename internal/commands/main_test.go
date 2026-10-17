package commands

import (
	"fmt"
	"os"
	"testing"
)

// TestMain runs the package's tests with TMPDIR a dir of their own: a
// test that runs `mesh node start` runs its start-up scratch sweep
// (node.SweepScratch) over os.TempDir(), which must never be the host's
// /tmp, where other sessions' dirs live.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mesh-commands-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("TMPDIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
