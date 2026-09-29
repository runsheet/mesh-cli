//go:build !windows

package validate

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts cmd in its own process group, so killGroup reaches
// every process it forks (a step's command is a shell script; killing
// only the shell leaves a still-running child, e.g. `sleep`, holding the
// output pipe open).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup sends SIGKILL to every process in pid's group.
func killGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
