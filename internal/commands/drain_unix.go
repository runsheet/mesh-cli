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

// signalDrain sends the drain signal to pid; tests replace it.
var signalDrain = func(pid int) error { return syscall.Kill(pid, syscall.SIGUSR1) }

// restartSignal is what `mesh node upgrade` sends a node no service
// manager runs, and `mesh node start` restarts on at once: its sessions
// are left running and the process execs its binary again (adr-009).
var restartSignal os.Signal = syscall.SIGHUP

// signalRestart sends the restart signal to pid.
func signalRestart(pid int) error { return syscall.Kill(pid, syscall.SIGHUP) }

// execSelf replaces this process with exe, same arguments and
// environment: the pid stays, so a service manager that runs it sees
// no exit.
func execSelf(exe string) error { return syscall.Exec(exe, os.Args, os.Environ()) }

// processAlive reports whether pid is a live process.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
