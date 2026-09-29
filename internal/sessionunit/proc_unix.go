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
// when the session or any of its group is still there after StopGrace,
// then waits goneSlack for the group to be gone: a group still standing,
// or a signal refused, is its error.
func stopGroup(h Handle) error {
	if h.PID <= 0 {
		return errors.New("sessionunit: no pid")
	}
	if err := syscall.Kill(-h.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("sessionunit: stop %d: %w", h.PID, err)
	}
	if waitGroupGone(h, StopGrace) {
		return nil
	}
	if err := syscall.Kill(-h.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("sessionunit: kill %d: %w", h.PID, err)
	}
	if waitGroupGone(h, goneSlack) {
		return nil
	}
	return fmt.Errorf("sessionunit: process group %d still there after SIGKILL", h.PID)
}

// waitGroupGone waits up to d for the session and its group to be gone.
func waitGroupGone(h Handle, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if groupGone(h) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// groupGone is whether the session has exited and no live process is
// left in its group. Where there is /proc its zombies do not count (an
// adopted session's parent is the node, which never reaps it); without
// one the group must be empty to kill(2).
func groupGone(h Handle) bool {
	if !h.Exited() {
		return false
	}
	if _, err := os.Stat("/proc/self/stat"); err == nil {
		return len(groupMembers(h.PID)) == 0
	}
	return errors.Is(syscall.Kill(-h.PID, 0), syscall.ESRCH)
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
