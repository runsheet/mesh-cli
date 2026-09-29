//go:build !windows

package sessionunit

import (
	"os"
	"syscall"
	"testing"
)

// A group the stop cannot end — a signal refused (EPERM) — is Kill's
// error and no report, though its members were read. The refusal is
// injected, and the group is this test's own sleeps: no other process
// is ever signalled.
func TestDetachedKillStopError(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	fastStop(t)
	h, cleanup := startSleeps(t, Detached{})
	defer cleanup()
	real := signalGroup
	var sent []int
	signalGroup = func(pid int, sig syscall.Signal) error {
		sent = append(sent, pid)
		return syscall.EPERM
	}
	defer func() { signalGroup = real }()
	killed, err := Detached{}.Kill(h)
	if err == nil || len(killed) != 0 {
		t.Fatalf("killed %v, err %v", killed, err)
	}
	for _, pid := range sent {
		if pid != h.PID {
			t.Fatalf("signalled group %d, not the session's %d", pid, h.PID)
		}
	}
	if len(sent) == 0 {
		t.Fatal("no signal attempted")
	}
}
