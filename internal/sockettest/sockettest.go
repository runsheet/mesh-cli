// Package sockettest gives a test a directory short enough to bind a
// unix socket under. sun_path is 108 bytes on Linux and 104 on macOS,
// and TMPDIR is not always short: a sandboxed validation's is the run's
// scratch, deep in the node's job dir
// (<home>/.mesh/jobs/<item id>/scratch/validate-…), and a socket under
// os.MkdirTemp("", …) there runs past the limit — `bind: invalid
// argument` (internal/node/validation.go, sandbox.ValidationCache).
package sockettest

import (
	"os"
	"runtime"
	"testing"
)

// MaxDir is the longest directory Dir returns from TMPDIR, leaving room
// for a few path elements and a socket name under it (a node MCP root's
// `px/<id>/s`) inside sun_path.
const MaxDir = 64

// Dir is a new directory for a test's sockets, removed when the test
// ends: under TMPDIR when that is short enough, else under /tmp — inside
// a sandbox's pod that is the container's own, never the host's.
func Dir(t testing.TB, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(dir) > MaxDir && runtime.GOOS != "windows" {
		os.RemoveAll(dir)
		if dir, err = os.MkdirTemp("/tmp", prefix); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
