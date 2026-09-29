// Package crash makes a node that dies of a panic name its own death.
//
// Go's runtime prints an unrecovered panic's value and stack and exits
// 2, which systemd reports as status=2/INVALIDARGUMENT — its LSB name
// for the number, not a cause. On 2026-09-29 the mini's node died that
// way at 04:00:22Z (goal node-yaml-takes-effect-or-says-why-not). Guard,
// deferred at the top of `mesh node start` and of every goroutine the
// node starts, turns the panic into one greppable line and a status of
// its own: `mesh node: fatal: <value> (version <v>)`, then the stack,
// then exit ExitPanic. The service restarts on it as on any other exit
// (Restart=always, KeepAlive).
package crash

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"

	"projectmesh.io/mesh-cli/internal/version"
)

// ExitPanic is the status a node exits with when a panic reaches Guard:
// sysexits' EX_SOFTWARE, which systemd shows as status=70/SOFTWARE —
// never Go's own 2.
const ExitPanic = 70

var (
	mu sync.Mutex
	// Out is where the fatal line and the stack go: the node's stderr,
	// which its service appends to ~/.mesh/node.log. Tests replace it.
	Out io.Writer = os.Stderr
	// Exit ends the process; tests replace it.
	Exit = os.Exit
)

// Guard recovers a panic, writes Line and the stack to Out and exits
// ExitPanic. It must be deferred directly (`defer crash.Guard()`): only
// a deferred function's own recover stops a panic.
func Guard() {
	r := recover()
	if r == nil {
		return
	}
	stack := debug.Stack()
	// two goroutines panicking at once write one death, not two
	// interleaved: the second waits here until the first has exited
	mu.Lock()
	fmt.Fprintf(Out, "%s\n%s", Line(r), stack)
	Exit(ExitPanic)
	mu.Unlock()
}

// Line is the one line a panic of value r is logged as.
func Line(r any) string {
	return fmt.Sprintf("mesh node: fatal: %v (version %s)", r, version.Resolved())
}
