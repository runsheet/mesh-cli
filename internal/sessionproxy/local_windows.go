//go:build windows

package sessionproxy

import (
	"os/exec"
	"strconv"
	"syscall"
)

// setProcessGroup puts a local server in its own process group.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killGroup kills pid and its descendants (taskkill's /T walks the tree).
func killGroup(pid int) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
