package crash

import (
	"bytes"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/version"
)

// A panic under Guard is the whole fatal line, then the stack, then
// ExitPanic — not Go's 2.
func TestGuardNamesThePanic(t *testing.T) {
	restore(t)
	var out bytes.Buffer
	status := -1
	Out, Exit = &out, func(code int) { status = code }
	func() {
		defer Guard()
		var m map[string]int
		m["x"] = 1
	}()
	if status != ExitPanic || status == 2 {
		t.Fatalf("exit %d, want %d", status, ExitPanic)
	}
	lines := strings.Split(out.String(), "\n")
	if want := "mesh node: fatal: assignment to entry in nil map (version " + version.Resolved() + ")"; lines[0] != want {
		t.Fatalf("first line %q, want %q", lines[0], want)
	}
	if !strings.HasPrefix(lines[1], "goroutine ") || !strings.Contains(out.String(), "crash.TestGuardNamesThePanic") {
		t.Fatalf("no stack after the line:\n%s", out.String())
	}
}

// No panic, no line and no exit.
func TestGuardQuietWithoutPanic(t *testing.T) {
	restore(t)
	var out bytes.Buffer
	Out, Exit = &out, func(int) { t.Fatal("exited without a panic") }
	func() { defer Guard() }()
	if out.Len() != 0 {
		t.Fatalf("wrote %q", out.String())
	}
}

func restore(t *testing.T) {
	out, exit := Out, Exit
	t.Cleanup(func() { Out, Exit = out, exit })
}
