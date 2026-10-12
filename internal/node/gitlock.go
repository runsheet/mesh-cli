package node

import "regexp"

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
