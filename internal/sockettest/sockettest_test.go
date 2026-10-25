package sockettest

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A TMPDIR as deep as a sandboxed validation's scratch still yields a
// directory a socket binds under, nested as a node MCP root's are.
func TestDirIsShortUnderALongTMPDIR(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /tmp to fall back to on windows")
	}
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 40), "jobs",
		"0f9c2a51-7e1b-4d3c-9a8e-1b2c3d4e5f60", "scratch", "validate-0f9c2a51-1a2b3c4d")
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", long)
	dir := Dir(t, "sp")
	if len(dir) > MaxDir {
		t.Fatalf("Dir = %s (%d bytes), want at most %d", dir, len(dir), MaxDir)
	}
	nested := filepath.Join(dir, "px", "3006478971")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(nested, "s"))
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}
