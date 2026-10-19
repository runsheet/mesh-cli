//go:build !windows

package sessionproxy

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts a local server in its own process group, so
// killGroup reaches what it forks too (npx and uvx start the server as
// their own child).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup sends SIGKILL to every process in pid's group.
func killGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
