package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
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
// LockRoot when set, else the given root (an item spec's, its
// connection's directory), else the connection of the item or batch job
// dir is in (ADR-039), else dir's grandparent — the plan's own
// directory for a plan's <job>/plan/repos/<name>, the work root for a
// <root>/<goal>/<seq>.
func (g *Git) lockRoot(root, dir string) string {
	if g.LockRoot != "" {
		return g.LockRoot
	}
	if root != "" {
		return root
	}
	if c, ok := itemConnectionOf(dir); ok {
		return c.Dir
	}
	return filepath.Dir(filepath.Dir(dir))
}

// lockHolderKey carries on a ctx the label of the item a lockRepo call
// holds the lock for.
type lockHolderKey struct{}

// WithLockHolder marks ctx with the label of the item its per-repo locks
// are held for — `<goal>/<seq>` — so a session waiting on one can be told
// whose it is.
func WithLockHolder(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, lockHolderKey{}, label)
}

// lockHolderOf is the label WithLockHolder put on ctx, else "unknown".
func lockHolderOf(ctx context.Context) string {
	if l, ok := ctx.Value(lockHolderKey{}).(string); ok && l != "" {
		return l
	}
	return "unknown"
}

// lockRoleOf is the role of ctx's actor (`agent:<role>@<slug>`), else "".
func lockRoleOf(ctx context.Context) string {
	a, ok := gateway.ActorFrom(ctx)
	if !ok {
		return ""
	}
	a = strings.TrimPrefix(a, "agent:")
	if i := strings.Index(a, "@"); i >= 0 {
		a = a[:i]
	}
	return a
}

// lockHolder is who holds a per-repo lock: the item's label, the
// session's role, the process and since when. It is the one line written
// into the lock file once the flock is taken, and the entry repoHolders
// keeps for the in-process mutex.
type lockHolder struct {
	Label string
	Role  string
	PID   int
	Since time.Time
}

// line is the holder record as written into the lock file:
// `label=<l> role=<r> pid=<n> since=<RFC3339Nano>`.
func (h lockHolder) line() string {
	role := h.Role
	if role == "" {
		role = "-"
	}
	return fmt.Sprintf("label=%s role=%s pid=%d since=%s\n", h.Label, role, h.PID, h.Since.UTC().Format(time.RFC3339Nano))
}

// parseLockHolder reads a holder record back; ok is false for an empty
// or unreadable file — a lock taken by an older node.
func parseLockHolder(b []byte) (lockHolder, bool) {
	var h lockHolder
	line := strings.TrimSpace(string(b))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	for _, f := range strings.Fields(line) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "label":
			h.Label = v
		case "role":
			if v != "-" {
				h.Role = v
			}
		case "pid":
			h.PID, _ = strconv.Atoi(v)
		case "since":
			h.Since, _ = time.Parse(time.RFC3339Nano, v)
		}
	}
	return h, h.Label != ""
}

// waitingLine is what a waiting lockRepo logs about holder h:
// `waiting on <label> (pid <n>, held <d>)`.
func (h lockHolder) waitingLine(now time.Time) string {
	held := "unknown"
	if !h.Since.IsZero() {
		held = now.Sub(h.Since).Round(time.Millisecond).String()
	}
	return fmt.Sprintf("waiting on %s (pid %d, held %s)", h.Label, h.PID, held)
}

// repoHolders is who holds each in-process mutex of repoLocks, by key.
var repoHolders sync.Map // key → lockHolder

// repoLockLogf is the package's logger for the lock's waits: a lockRepo
// that has to wait logs whom it waits on and, once it has the lock, how
// long it waited. Nil logs nothing; SetRepoLockLogf sets it.
var repoLockLogf atomic.Pointer[func(format string, args ...any)]

// SetRepoLockLogf sets the logger a waiting lockRepo writes its lines to
// (node.log's, set by mesh node start); nil silences it.
func SetRepoLockLogf(logf func(format string, args ...any)) {
	if logf == nil {
		repoLockLogf.Store(nil)
		return
	}
	repoLockLogf.Store(&logf)
}

func lockLogf(format string, args ...any) {
	if p := repoLockLogf.Load(); p != nil {
		(*p)(format, args...)
	}
}

// lockRepo takes the per-repo lock: the process's mutex for key, then an
// exclusive flock on its file under root, so two node processes on one
// machine are serialised too. The returned func releases both. Once both
// are held the holder — ctx's WithLockHolder label, its actor's role,
// the pid and the time — is written into the lock file (the flock is on
// the fd, not the content) and kept beside the mutex. A call that has to
// wait on either logs `repo lock <key>: waiting on <holder> (pid <n>,
// held <d>)` and, once it has the lock, `repo lock <key>: acquired after
// <d>`; a free lock logs nothing.
func lockRepo(ctx context.Context, root, key string) (func(), error) {
	start := time.Now()
	waited := false
	v, _ := repoLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		waited = true
		h, ok := repoHolders.Load(key)
		if !ok {
			h = lockHolder{Label: "unknown", PID: os.Getpid()}
		}
		lockLogf("repo lock %s: %s", key, h.(lockHolder).waitingLine(time.Now()))
		mu.Lock()
	}
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
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			f.Close()
			mu.Unlock()
			return nil, fmt.Errorf("repo lock %s: %w", path, err)
		}
		waited = true
		b, _ := os.ReadFile(path)
		h, ok := parseLockHolder(b)
		if !ok {
			h = lockHolder{Label: "unknown"}
		}
		lockLogf("repo lock %s: %s", key, h.waitingLine(time.Now()))
		if err := flockCtx(ctx, f); err != nil {
			f.Close()
			mu.Unlock()
			return nil, fmt.Errorf("repo lock %s: %w", path, err)
		}
	}
	holder := lockHolder{Label: lockHolderOf(ctx), Role: lockRoleOf(ctx), PID: os.Getpid(), Since: time.Now()}
	repoHolders.Store(key, holder)
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(holder.line()), 0)
	}
	if waited {
		lockLogf("repo lock %s: acquired after %s", key, time.Since(start).Round(time.Millisecond))
	}
	return func() {
		repoHolders.Delete(key)
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
