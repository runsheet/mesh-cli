package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// LocksDir is where the per-repo fetch locks live under a work root:
// <root>/.locks/<owner>--<name>.lock (ADR-023, fetches of one repo are
// serialised on the node).
const LocksDir = ".locks"

// repoLocks is the in-process half of the per-repo lock: one mutex per
// key, shared by every Git of the process, since the runner, evaluator,
// rebaser, planner and director each hold their own.
var repoLocks sync.Map // key → *sync.Mutex

// flockPoll is how often a waiting fetch tries the file lock again.
const flockPoll = 50 * time.Millisecond

// RepoLockKey is the key the per-repo lock is held under: owner/name for
// a GitHub repo however it is spelt (owner/name, an https or ssh URL),
// lower-cased; any other remote — a path, as tests pass — as itself.
func RepoLockKey(repo string) string {
	r := strings.TrimSuffix(strings.TrimSpace(repo), "/")
	r = strings.TrimSuffix(r, ".git")
	for _, p := range []string{"https://github.com/", "http://github.com/", "ssh://git@github.com/", "git@github.com:"} {
		if strings.HasPrefix(r, p) {
			return strings.ToLower(strings.TrimPrefix(r, p))
		}
	}
	if !strings.Contains(r, ":") && !strings.HasPrefix(r, "/") && !strings.HasPrefix(r, ".") && strings.Count(r, "/") == 1 {
		return strings.ToLower(r)
	}
	return r
}

// RepoLockPath is the lock file for key under root: owner/name becomes
// owner--name.lock; any other key is made a file name by the same rule,
// its separators and colons replaced.
func RepoLockPath(root, key string) string {
	name := strings.NewReplacer("/", "--", "\\", "--", ":", "_").Replace(strings.Trim(key, "/"))
	return filepath.Join(root, LocksDir, name+".lock")
}

// lockRoot is the root the lock files of dir's repo live under:
// LockRoot when set, else the given work root, else dir's grandparent —
// ~/.mesh/work for an item's ~/.mesh/work/<goal>/<seq>, the plan's own
// directory for a plan's PlanDir/repos/<name>.
func (g *Git) lockRoot(root, dir string) string {
	if g.LockRoot != "" {
		return g.LockRoot
	}
	if root != "" {
		return root
	}
	return filepath.Dir(filepath.Dir(dir))
}

// lockRepo takes the per-repo lock: the process's mutex for key, then an
// exclusive flock on its file under root, so two node processes on one
// machine are serialised too. The returned func releases both.
func lockRepo(ctx context.Context, root, key string) (func(), error) {
	v, _ := repoLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	path := RepoLockPath(root, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("repo lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("repo lock: %w", err)
	}
	if err := flockCtx(ctx, f); err != nil {
		f.Close()
		mu.Unlock()
		return nil, fmt.Errorf("repo lock %s: %w", path, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		mu.Unlock()
	}, nil
}

// flockCtx takes LOCK_EX on f, giving up when ctx is done: a blocking
// flock in a goroutine would outlive a cancelled session, so it polls a
// non-blocking one instead.
func flockCtx(ctx context.Context, f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(flockPoll):
		}
	}
}

// repoKeyOf is the lock key of dir's checkout, from its origin's URL; a
// checkout with no origin is keyed by its own path, a lock of its own.
func (g *Git) repoKeyOf(ctx context.Context, dir string) string {
	if out, err := g.run(ctx, dir, "git", "remote", "get-url", "origin"); err == nil && out != "" {
		return RepoLockKey(out)
	}
	abs, _ := filepath.Abs(dir)
	return abs
}

// fetchLocked runs one command that writes refs from the remote — a
// `git fetch` or a `gh repo clone` — in dir under the per-repo lock of
// key at root, released as soon as it returns: never held across a
// checkout, a session or a push. Every such command of Git goes through
// here, so a retry has one place to live.
func (g *Git) fetchLocked(ctx context.Context, root, key, dir, name string, args ...string) (string, error) {
	unlock, err := lockRepo(ctx, root, key)
	if err != nil {
		return "", err
	}
	defer unlock()
	return g.run(ctx, dir, name, args...)
}
