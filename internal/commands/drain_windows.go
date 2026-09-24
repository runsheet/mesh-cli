//go:build windows

package commands

import (
	"errors"
	"os"
)

// drainSignal is nil: Windows has no SIGUSR1, so a node there cannot be
// drained, only stopped.
var drainSignal os.Signal

func signalDrain(int) error { return errors.New("mesh node drain is not supported on Windows") }

func processAlive(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}
