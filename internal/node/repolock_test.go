package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// captureRepoLockLog points the lock's logger at a buffer for the test.
func captureRepoLockLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	SetRepoLockLogf(func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	t.Cleanup(func() { SetRepoLockLogf(nil) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "\n")
	}
}

func TestLockRepoWaiterNamesInProcessHolder(t *testing.T) {
	logged := captureRepoLockLog(t)
	root := t.TempDir()
	key := "acme/inproc-holder"

	unlockA, err := lockRepo(WithLockHolder(context.Background(), "g/1"), root, key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(RepoLockPath(root, key))
	if h, ok := parseLockHolder(b); !ok || h.Label != "g/1" || h.PID != os.Getpid() {
		t.Fatalf("lock file holder = %q, want g/1 and pid %d", b, os.Getpid())
	}

	acquired := make(chan func())
	go func() {
		unlockB, err := lockRepo(WithLockHolder(context.Background(), "g/2"), root, key)
		if err != nil {
			t.Error(err)
		}
		acquired <- unlockB
	}()
	waitFor(t, func() bool { return strings.Contains(logged(), "waiting on g/1") })
	select {
	case <-acquired:
		t.Fatal("B acquired while A held the lock")
	case <-time.After(50 * time.Millisecond):
	}
	if !strings.Contains(logged(), "repo lock "+key+": waiting on g/1 (pid ") {
		t.Fatalf("log = %q", logged())
	}

	unlockA()
	unlockB := <-acquired
	defer unlockB()
	if !strings.Contains(logged(), "repo lock "+key+": acquired after ") {
		t.Fatalf("log = %q, want an acquired line", logged())
	}
	b, _ = os.ReadFile(RepoLockPath(root, key))
	if h, _ := parseLockHolder(b); h.Label != "g/2" {
		t.Fatalf("lock file holder after B = %q, want g/2", b)
	}
}

func TestLockRepoWaiterNamesHolderOfFileLock(t *testing.T) {
	logged := captureRepoLockLog(t)
	root := t.TempDir()
	key := "acme/flock-holder"
	path := RepoLockPath(root, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// another process's lock: its own open file description, flocked,
	// with its holder line written
	other, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-3 * time.Second)
	other.WriteString(lockHolder{Label: "other-goal/7", Role: "builder", PID: 424242, Since: since}.line())

	acquired := make(chan func())
	go func() {
		unlock, err := lockRepo(WithLockHolder(context.Background(), "g/2"), root, key)
		if err != nil {
			t.Error(err)
		}
		acquired <- unlock
	}()
	waitFor(t, func() bool { return strings.Contains(logged(), "waiting on other-goal/7") })
	if !strings.Contains(logged(), "repo lock "+key+": waiting on other-goal/7 (pid 424242, held 3") {
		t.Fatalf("log = %q", logged())
	}
	syscall.Flock(int(other.Fd()), syscall.LOCK_UN)
	unlock := <-acquired
	defer unlock()
	if !strings.Contains(logged(), "repo lock "+key+": acquired after ") {
		t.Fatalf("log = %q, want an acquired line", logged())
	}
}

func TestLockRepoUncontendedLogsNothing(t *testing.T) {
	logged := captureRepoLockLog(t)
	root := t.TempDir()
	for i := 0; i < 2; i++ {
		unlock, err := lockRepo(WithLockHolder(context.Background(), "g/1"), root, "acme/free")
		if err != nil {
			t.Fatal(err)
		}
		unlock()
	}
	if got := logged(); got != "" {
		t.Fatalf("uncontended lockRepo logged %q", got)
	}
}
