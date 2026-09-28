//go:build !windows

package sessionunit

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// detach puts the session in its own session and process group, so a
// signal to the node's group, or the node's exit, does not reach it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// stopGroup sends SIGTERM to the session's process group, and SIGKILL
// when the session or any of its group is still there after StopGrace.
func stopGroup(h Handle) error {
	if h.PID <= 0 {
		return errors.New("sessionunit: no pid")
	}
	if err := syscall.Kill(-h.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("sessionunit: stop %d: %w", h.PID, err)
	}
	deadline := time.Now().Add(StopGrace)
	for time.Now().Before(deadline) {
		if h.Exited() && errors.Is(syscall.Kill(-h.PID, 0), syscall.ESRCH) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-h.PID, syscall.SIGKILL)
	return nil
}

// pidAlive is whether pid is a running process (not a zombie).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if st, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "stat")); err == nil {
		// "pid (comm) S ...": the state follows the last ')'
		if i := strings.LastIndexByte(string(st), ')'); i >= 0 && i+2 < len(st) && st[i+2] == 'Z' {
			return false
		}
	}
	return true
}
