//go:build windows

package sessionunit

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// detach starts the session in its own process group, so a console
// Ctrl+C to the node does not reach it. Windows has no systemd, so
// Default is always Detached here.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// stopGroup kills the session's process; Windows has no SIGTERM to
// offer first.
func stopGroup(h Handle) error {
	if h.PID <= 0 {
		return errors.New("sessionunit: no pid")
	}
	p, err := os.FindProcess(h.PID)
	if err != nil {
		return nil // already gone
	}
	if err := p.Kill(); err != nil && pidAlive(h.PID) {
		return fmt.Errorf("sessionunit: stop %d: %w", h.PID, err)
	}
	deadline := time.Now().Add(StopGrace)
	for time.Now().Before(deadline) && !h.Exited() {
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// pidAlive is whether pid is a running process.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInformation = 0x1000
	const stillActive = 259
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
