//go:build !windows

package commands

import (
	"errors"
	"os"
	"syscall"
)

// drainSignal is what `mesh node drain` sends and `mesh node start`
// drains on.
var drainSignal os.Signal = syscall.SIGUSR1

// signalDrain sends the drain signal to pid.
func signalDrain(pid int) error { return syscall.Kill(pid, syscall.SIGUSR1) }

// processAlive reports whether pid is a live process.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
