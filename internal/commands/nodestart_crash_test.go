package commands

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/crash"
	"projectmesh.io/mesh-cli/internal/version"
)

// The child half of TestNodeStartPanicNamesItself: `mesh node start`
// whose start path panics as the mini's did at 2026-09-29 04:00:22Z.
func TestNodeStartPanicChild(t *testing.T) {
	if os.Getenv("MESH_TEST_NODE_START_PANIC") != "1" {
		t.Skip("run by TestNodeStartPanicNamesItself")
	}
	startNode = func(*cobra.Command, []string) error {
		panic("runtime error: invalid memory address or nil pointer dereference")
	}
	rootCmd.SetArgs([]string{"node", "start", "--mode=poll", "--unattended"})
	_ = rootCmd.Execute()
	t.Fatal("node start returned after a panic")
}

// A panic inside `mesh node start` is one whole line in node.log —
// `mesh node: fatal: <value> (version <v>)` — then its stack, and the
// process exits crash.ExitPanic (70), distinct from Go's 2, which
// systemd names INVALIDARGUMENT.
func TestNodeStartPanicNamesItself(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestNodeStartPanicChild$")
	cmd.Env = append(os.Environ(), "MESH_TEST_NODE_START_PANIC=1", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if code := exit.ExitCode(); code != crash.ExitPanic || code == 2 {
		t.Fatalf("exit status %d, want %d:\n%s", code, crash.ExitPanic, out)
	}
	lines := strings.Split(string(out), "\n")
	want := "mesh node: fatal: runtime error: invalid memory address or nil pointer dereference (version " + version.Resolved() + ")"
	if lines[0] != want {
		t.Fatalf("first line %q, want %q\n%s", lines[0], want, out)
	}
	if !strings.HasPrefix(lines[1], "goroutine ") || !strings.Contains(string(out), "TestNodeStartPanicChild") {
		t.Fatalf("no stack after the fatal line:\n%s", out)
	}
}
