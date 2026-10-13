//go:build !windows

package sessionunit

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

// A real detached session whose agent started a child of its own — the
// MCP helper or the background server it left — is gone with that child
// after Kill, within the stop grace and a bounded wait: the kill is the
// whole process group's, not the leader's alone (goal
// a-stalled-session-is-stopped-not-orphaned).
func TestDetachedKillEndsParentAndChild(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	fastStop(t)
	dir := t.TempDir()
	childFile := filepath.Join(dir, "child.pid")
	out, _ := os.Create(filepath.Join(dir, "out"))
	errf, _ := os.Create(filepath.Join(dir, "err"))
	// the parent ignores SIGTERM, so the kill must also reach it by SIGKILL
	script := "trap '' TERM; sleep 300 & echo $! > " + childFile + "; while :; do sleep 1; done"
	h, err := Detached{}.Start(Spec{Kind: "test", Subject: "tree", Binary: "sh", Args: []string{"-c", script}, Dir: dir, Env: os.Environ(), Stdout: out, Stderr: errf})
	out.Close()
	errf.Close()
	if err != nil {
		t.Fatal(err)
	}
	var child int
	deadline := time.Now().Add(5 * time.Second)
	for child == 0 {
		if b, err := os.ReadFile(childFile); err == nil {
			child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-h.PID, syscall.SIGKILL)
		_ = syscall.Kill(child, syscall.SIGKILL)
	})
	if !pidAlive(h.PID) || !pidAlive(child) {
		t.Fatalf("parent %d alive %v, child %d alive %v", h.PID, pidAlive(h.PID), child, pidAlive(child))
	}
	start := time.Now()
	killed, err := Detached{}.Kill(h)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range killed {
		found = found || p.PID == child
	}
	if !found {
		t.Fatalf("killed %v does not name the child %d", killed, child)
	}
	bound := StopGrace + goneSlack + 2*time.Second
	for pidAlive(h.PID) || pidAlive(child) {
		if time.Since(start) > bound {
			t.Fatalf("after %v: parent alive %v, child alive %v", bound, pidAlive(h.PID), pidAlive(child))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
