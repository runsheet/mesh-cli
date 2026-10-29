package actor

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// Killed answers the exit code, and true, when err is a process or a
// container ended by SIGTERM or SIGKILL: exit 143 or 137, or the signal
// itself. Whether that is pod_killed — never red — or the actor's own
// stop is the caller's to say: an actor that cancelled the lease caused
// it, any other is someone else's kill (the OOM killer, an operator, a
// runtime that removed the pod) and the lease is handed back pod_killed
// (ADR-044).
func Killed(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var se *sandbox.ExitError
	if errors.As(err, &se) {
		return se.Code, killedCode(se.Code)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			switch ws.Signal() {
			case syscall.SIGKILL:
				return 137, true
			case syscall.SIGTERM:
				return 143, true
			}
		}
		return ee.ExitCode(), killedCode(ee.ExitCode())
	}
	// a session's error carries the process's end as text (nodev1's
	// ClaudeSession: "claude: signal: killed")
	msg := err.Error()
	for _, k := range []struct {
		text string
		code int
	}{{"signal: killed", 137}, {"signal: terminated", 143}, {"exit status 137", 137}, {"exit status 143", 143}} {
		if strings.HasSuffix(msg, k.text) {
			return k.code, true
		}
	}
	return 0, false
}

// KilledExit is Killed for an exit code read from the runtime.
func KilledExit(code int) bool { return killedCode(code) }

func killedCode(code int) bool { return code == 137 || code == 143 }
