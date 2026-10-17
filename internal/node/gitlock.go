package node

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// configLockRe matches git's "could not lock config file <path>: File
// exists", the message IsGitLockFailure saw on a stale .git/config.lock in
// the node's own repo cache (goal a-stale-git-lock-does-not-fail-every-item).
// The path git names is the config file itself; the lock it left behind is
// that path with ".lock" appended.
var configLockRe = regexp.MustCompile(`could not lock config file (\S+): File exists`)

// createLockRe matches git's "Unable to create '<path>.lock': File exists",
// the message for index.lock, HEAD.lock and a ref's own lock
// (refs/heads/<branch>.lock and the like). Here the path git names is the
// lock file itself.
var createLockRe = regexp.MustCompile(`Unable to create '([^']+)': File exists`)

// IsGitLockFailure reports whether err is git refusing to run because a lock
// file from an earlier, no-longer-running process is still on disk — the
// config, index, HEAD or a ref's own lock — rather than a failure in the
// command itself. nil is false.
func IsGitLockFailure(err error) bool {
	if err == nil {
		return false
	}
	return IsGitLockFailureText(err.Error())
}

// IsGitLockFailureText is the string form IsGitLockFailure wraps, for a
// caller holding git's output rather than an error.
func IsGitLockFailureText(s string) bool {
	_, ok := GitLockPath(s)
	return ok
}

// GitLockPath extracts the lock file's own path from git's lock-contention
// message, when msg carries one: "could not lock config file <path>: File
// exists" (the lock is <path>.lock) or "Unable to create '<path>.lock':
// File exists" (index.lock, HEAD.lock, a ref's own lock — the path is the
// lock file already). An unrelated message is false.
func GitLockPath(msg string) (string, bool) {
	if m := configLockRe.FindStringSubmatch(msg); m != nil {
		return m[1] + ".lock", true
	}
	if m := createLockRe.FindStringSubmatch(msg); m != nil {
		return m[1], true
	}
	return "", false
}

// StaleGitLockAge is how long a git lock file sits untouched before
// ClearStaleGitLock treats it as abandoned rather than contended: a
// crashed or killed process's lock outlives it, and nothing else on the
// node ever removes it (goal
// a-shared-clone-is-never-written-by-two-sessions-at-once).
const StaleGitLockAge = time.Minute

// ClearStaleGitLock reads msg as git's lock-contention message (as
// GitLockPath does) and, when the lock file it names — resolved
// against dir when git named it relative to the command's own working
// directory, as it does for a checkout's own ".git/config" (a worktree's
// config.worktree is always named absolute) — is on disk and older than
// maxAge as of now, removes it and logs through logf (nil, silent)
// "removed stale git lock <path> (age <d>)", reporting true. A lock
// younger than maxAge — a write genuinely still running — is left in
// place and reported false, as is a message GitLockPath finds nothing
// in, or a lock file already gone.
func ClearStaleGitLock(dir, msg string, maxAge time.Duration, now time.Time, logf func(format string, args ...any)) bool {
	path, ok := GitLockPath(msg)
	if !ok {
		return false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	age := now.Sub(info.ModTime())
	if age < maxAge {
		return false
	}
	if err := os.Remove(path); err != nil {
		return false
	}
	if logf != nil {
		logf("removed stale git lock %s (age %s)", path, age.Round(time.Second))
	}
	return true
}

// PreclearGitLockAge is how long dir's repository config.lock sits
// untouched before withStaleLockRetry removes it before fn is ever
// tried, rather than waiting for a write to fail on it first — longer
// than StaleGitLockAge, because nothing has failed yet to say the lock
// is a problem, only its age and an empty-handed scan for a git process
// still working (goal a-workspace-whose-worktree-is-gone-is-rebuilt: a
// workspace cleanup racing a live or resumed session can leave the lock
// there well before the next write ever sees it).
const PreclearGitLockAge = 5 * time.Minute

// gitLockHolder reports whether a live git process on this host could be
// the one holding a lock: one whose current directory or GIT_DIR
// environment variable resolves under any of roots. A package var, not
// a method, so a test stubs it without a real /proc scan;
// gitLockHolderProc is the real one.
var gitLockHolder = gitLockHolderProc

// gitLockHolderProc scans /proc for a live process named "git" whose cwd
// or GIT_DIR falls under one of roots. A host with no /proc (not linux,
// where the node never runs sessions) or a scan it cannot read reports
// no holder, leaving the age check as the only gate.
func gitLockHolderProc(roots ...string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		pidDir := filepath.Join("/proc", e.Name())
		comm, err := os.ReadFile(filepath.Join(pidDir, "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "git" {
			continue
		}
		if cwd, err := os.Readlink(filepath.Join(pidDir, "cwd")); err == nil && underAnyDir(cwd, roots) {
			return true
		}
		environ, err := os.ReadFile(filepath.Join(pidDir, "environ"))
		if err != nil {
			continue
		}
		for _, kv := range strings.Split(string(environ), "\x00") {
			if val, ok := strings.CutPrefix(kv, "GIT_DIR="); ok && underAnyDir(val, roots) {
				return true
			}
		}
	}
	return false
}

// underAnyDir reports whether path is one of roots or inside one of them.
func underAnyDir(path string, roots []string) bool {
	for _, root := range roots {
		if root == "" || path == "" {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// commonDirConfigLock resolves dir's repository common dir — `git
// rev-parse --git-common-dir`, so a worktree's own lock is the shared
// clone's .git/config.lock, not a config.worktree it never has one of —
// and returns its config.lock path.
func (g *Git) commonDirConfigLock(dir string) (string, error) {
	out, err := g.run(context.Background(), dir, "git", "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	common := strings.TrimSpace(out)
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	return filepath.Join(common, "config.lock"), nil
}

// preclearStaleLock checks dir's repository config.lock before fn ever
// runs: older than PreclearGitLockAge and no live git process holding it
// (gitLockHolder, over dir and the lock's own directory) is removed and
// logged the same way ClearStaleGitLock reports an after-the-failure
// removal. A lock younger than that, one a process holds, or a
// repository git rev-parse cannot resolve, is left for that ordinary,
// after-the-failure path.
func (g *Git) preclearStaleLock(dir string) {
	path, err := g.commonDirConfigLock(dir)
	if err != nil {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	age := time.Since(info.ModTime())
	if age < PreclearGitLockAge {
		return
	}
	if gitLockHolder(dir, filepath.Dir(path)) {
		return
	}
	if err := os.Remove(path); err != nil {
		return
	}
	if g.Logf != nil {
		g.Logf("removed stale git lock %s (age %s)", path, age.Round(time.Second))
	}
}

// withStaleLockRetry runs fn — a write already wrapped by Git.run in dir,
// so a lock-contention failure is text in its own err.Error() — after
// preclearStaleLock has had a chance to clear dir's config.lock
// unconditionally (no failure needed first), and, on such a failure
// anyway, clears the lock when ClearStaleGitLock finds it stale and runs
// fn once more. Any other failure, or a lock too young to clear (a live
// write, not an abandoned one), returns fn's first result unchanged, so
// a caller still releases it as an infrastructure hold (ADR-024) just as
// before this retry existed.
func (g *Git) withStaleLockRetry(dir string, fn func() (string, error)) (string, error) {
	g.preclearStaleLock(dir)
	out, err := fn()
	if err == nil || !IsGitLockFailure(err) {
		return out, err
	}
	if !ClearStaleGitLock(dir, err.Error(), StaleGitLockAge, time.Now(), g.Logf) {
		return out, err
	}
	return fn()
}
