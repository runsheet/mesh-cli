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
	return filepath.Join(root, LocksDir, repoFileName(key)+".lock")
}

// repoFileName is key made one file name: its separators and colons
// replaced, owner/name becoming owner--name.
func repoFileName(key string) string {
	return strings.NewReplacer("/", "--", "\\", "--", ":", "_").Replace(strings.Trim(key, "/"))
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

// refLockRetries is how many times a fetch or clone is tried before
// giving up on a ref lock failure (ADR-023's Retry section).
const refLockRetries = 3

// refLockBackoff is the wait before each retry, one per retry.
var refLockBackoff = []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}

// refLockSleep waits d or until ctx ends; tests substitute it, as
// credentialSleep does in ghcred.go.
var refLockSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// fetchLocked runs one command that writes refs from the remote — a
// `git fetch` or a `gh repo clone` — in dir under the per-repo lock of
// key at root, released as soon as it returns: never held across a
// checkout, a session or a push. Every such command of Git goes through
// here, so a retry has one place to live: a failure IsRefLockFailure is
// retried up to refLockRetries times with refLockBackoff between tries —
// another session fetching the same repo at the same moment, not a
// failure of this one — and logged once, when it finally succeeds or
// finally gives up (ADR-023's Retry section). A cancelled ctx stops the
// retry; any other error is returned at once.
func (g *Git) fetchLocked(ctx context.Context, root, key, dir, name string, args ...string) (string, error) {
	unlock, err := lockRepo(ctx, root, key)
	if err != nil {
		return "", err
	}
	defer unlock()
	return g.fetchRetrying(ctx, key, dir, name, args...)
}

// fetchRetrying is fetchLocked's body, for a caller already holding the
// per-repo lock of key: the command run with the ref-lock retry.
func (g *Git) fetchRetrying(ctx context.Context, key, dir, name string, args ...string) (string, error) {
	var out string
	var err error
	var lastErr error
	fails := 0
	for attempt := 1; attempt <= refLockRetries; attempt++ {
		out, err = g.run(ctx, dir, name, args...)
		if err == nil {
			if fails > 0 {
				g.logRefLockRetry(dir, key, fails, fmt.Sprintf("succeeded on try %d", attempt), lastErr)
			}
			return out, nil
		}
		if !IsRefLockFailure(err) {
			return out, err
		}
		fails++
		lastErr = err
		if attempt == refLockRetries {
			g.logRefLockRetry(dir, key, fails, fmt.Sprintf("gave up after %d tries", refLockRetries), lastErr)
			return out, err
		}
		if ctx.Err() != nil {
			return out, err
		}
		if sleepErr := refLockSleep(ctx, refLockBackoff[attempt-1]); sleepErr != nil {
			return out, err
		}
	}
	return out, lastErr
}

// logRefLockRetry writes fetchLocked's one line about a retried fetch:
// "workspace <dir>: git refused a ref lock for <repo> <n> time(s),
// <outcome>: <last error>".
func (g *Git) logRefLockRetry(dir, repo string, fails int, outcome string, lastErr error) {
	if g.Logf == nil {
		return
	}
	g.Logf("workspace %s: git refused a ref lock for %s %d time(s), %s: %v", dir, repo, fails, outcome, lastErr)
}
