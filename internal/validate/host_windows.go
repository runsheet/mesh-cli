//go:build windows

package validate

import (
	"os/exec"
	"strconv"
	"syscall"
)

// setProcessGroup puts cmd in its own process group.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killGroup kills pid and its descendants (Windows has no process-group
// signal; taskkill's /T walks the tree).
func killGroup(pid int) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
