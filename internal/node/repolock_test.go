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

// Two items of one repo whose specs name different job roots share one
// clone, so every write to it locks under the clone's own root — not
// each spec's — and the two wait on one lock file (ADR-023).
func TestSharedCloneLocksUnderItsOwnRoot(t *testing.T) {
	ctx := context.Background()
	reposRoot := filepath.Join(t.TempDir(), "repos")
	g := &Git{}
	a := WorkspaceSpec{Root: filepath.Join(t.TempDir(), "jobs", "a"), Repos: reposRoot, GoalSlug: "g", Seq: 1, Repo: "Acme/Widgets", Branch: "mesh/g-1", BaseRef: "main"}
	b := WorkspaceSpec{Root: filepath.Join(t.TempDir(), "jobs", "b"), Repos: reposRoot, GoalSlug: "g", Seq: 2, Repo: "acme/widgets", Branch: "mesh/g-2", BaseRef: "main"}
	if g.seedClone(a) == "" || g.seedClone(a) != g.seedClone(b) {
		t.Fatalf("shared clones %q and %q, want one", g.seedClone(a), g.seedClone(b))
	}
	ra, rb := g.specLockRoot(ctx, a), g.specLockRoot(ctx, b)
	if ra != rb {
		t.Fatalf("lock roots %q and %q for one shared clone, want one", ra, rb)
	}
	if ra != reposRoot {
		t.Fatalf("lock root %q, want the clone's repos root %q", ra, reposRoot)
	}
	if ra == g.lockRoot(a.Root, a.Dir()) {
		t.Fatalf("lock root %q is the spec's own root", ra)
	}
	if got := g.sharedLockRoot(g.seedClone(a), a.Repo); got != reposRoot {
		t.Fatalf("sharedLockRoot = %q, want %q", got, reposRoot)
	}
	// a path remote's clone is one directory under the root
	if got := g.sharedLockRoot(SharedClonePath(reposRoot, "/tmp/x/origin.git"), "/tmp/x/origin.git"); got != reposRoot {
		t.Fatalf("sharedLockRoot of a path remote = %q, want %q", got, reposRoot)
	}
	if got := (&Git{LockRoot: "/locks"}).specLockRoot(ctx, a); got != "/locks" {
		t.Fatalf("LockRoot set: %q, want /locks", got)
	}
}

// Two jobs of one repo each lock under their own root (ADR-039: no clone
// is shared), so one job's held lock never holds the other's back: the
// in-process mutex is keyed by root and repo, as the flock's file is.
func TestRepoLockOfTwoJobsDoesNotWait(t *testing.T) {
	logged := captureRepoLockLog(t)
	jobs := t.TempDir()
	rootA, rootB := filepath.Join(jobs, "job-a"), filepath.Join(jobs, "job-b")
	key := "acme/two-jobs"

	unlockA, err := lockRepo(WithLockHolder(context.Background(), "g/1"), rootA, key)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockA()

	acquired := make(chan func(), 1)
	go func() {
		unlockB, err := lockRepo(WithLockHolder(context.Background(), "g/2"), rootB, key)
		if err != nil {
			t.Error(err)
		}
		acquired <- unlockB
	}()
	select {
	case unlockB := <-acquired:
		unlockB()
	case <-time.After(2 * time.Second):
		t.Fatalf("job B waited on job A's lock of %s; log = %q", key, logged())
	}
	if got := logged(); got != "" {
		t.Fatalf("two jobs' locks logged %q, want no wait", got)
	}
}

// Under one job's root the lock still serialises: a second command of
// the same repo waits until the first releases it.
func TestRepoLockOfOneJobStillSerialises(t *testing.T) {
	logged := captureRepoLockLog(t)
	root := filepath.Join(t.TempDir(), "job-a")
	key := "acme/one-job"

	unlockA, err := lockRepo(WithLockHolder(context.Background(), "g/1"), root, key)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() {
		unlockB, err := lockRepo(WithLockHolder(context.Background(), "g/1"), root, key)
		if err != nil {
			t.Error(err)
		}
		acquired <- unlockB
	}()
	waitFor(t, func() bool { return strings.Contains(logged(), "repo lock "+key+": waiting on g/1") })
	select {
	case <-acquired:
		t.Fatal("a second lock of one job's repo was acquired while the first was held")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	unlockB := <-acquired
	unlockB()
	if !strings.Contains(logged(), "repo lock "+key+": acquired after ") {
		t.Fatalf("log = %q, want an acquired line", logged())
	}
}
