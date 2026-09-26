//go:build windows

package commands

import (
	"errors"
	"os"
)

// drainSignal is nil: Windows has no SIGUSR1, so a node there cannot be
// drained, only stopped.
var drainSignal os.Signal

var signalDrain = func(int) error { return errors.New("mesh node drain is not supported on Windows") }

// restartSignal is nil: a node there restarts only by its service.
var restartSignal os.Signal

func signalRestart(int) error {
	return errors.New("restarting a node by signal is not supported on Windows")
}

func execSelf(string) error { return errors.New("exec is not supported on Windows") }

func processAlive(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}
