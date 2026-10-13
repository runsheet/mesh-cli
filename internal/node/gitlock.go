package node

import (
	"os"
	"path/filepath"
	"regexp"
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

// withStaleLockRetry runs fn — a write already wrapped by Git.run in dir,
// so a lock-contention failure is text in its own err.Error() — and, on
// such a failure, clears the lock when ClearStaleGitLock finds it stale
// and runs fn once more. Any other failure, or a lock too young to clear
// (a live write, not an abandoned one), returns fn's first result
// unchanged, so a caller still releases it as an infrastructure hold
// (ADR-024) just as before this retry existed.
func (g *Git) withStaleLockRetry(dir string, fn func() (string, error)) (string, error) {
	out, err := fn()
	if err == nil || !IsGitLockFailure(err) {
		return out, err
	}
	if !ClearStaleGitLock(dir, err.Error(), StaleGitLockAge, time.Now(), g.Logf) {
		return out, err
	}
	return fn()
}
