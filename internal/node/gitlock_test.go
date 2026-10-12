package node

import (
	"errors"
	"testing"
)

func TestGitLockFailureConfigLock(t *testing.T) {
	err := errors.New("error: could not lock config file /x/.git/config: File exists")
	if !IsGitLockFailure(err) {
		t.Fatalf("IsGitLockFailure(%q) = false, want true", err)
	}
	path, ok := GitLockPath(err.Error())
	if !ok || path != "/x/.git/config.lock" {
		t.Fatalf("GitLockPath(%q) = %q, %v, want %q, true", err, path, ok, "/x/.git/config.lock")
	}
}

func TestGitLockFailureIndexLock(t *testing.T) {
	err := errors.New("fatal: Unable to create '/x/.git/index.lock': File exists.")
	if !IsGitLockFailure(err) {
		t.Fatalf("IsGitLockFailure(%q) = false, want true", err)
	}
	path, ok := GitLockPath(err.Error())
	if !ok || path != "/x/.git/index.lock" {
		t.Fatalf("GitLockPath(%q) = %q, %v, want %q, true", err, path, ok, "/x/.git/index.lock")
	}
}

func TestGitLockFailureHeadLock(t *testing.T) {
	err := errors.New("fatal: Unable to create '/x/.git/HEAD.lock': File exists.")
	if !IsGitLockFailure(err) {
		t.Fatalf("IsGitLockFailure(%q) = false, want true", err)
	}
	path, ok := GitLockPath(err.Error())
	if !ok || path != "/x/.git/HEAD.lock" {
		t.Fatalf("GitLockPath(%q) = %q, %v, want %q, true", err, path, ok, "/x/.git/HEAD.lock")
	}
}

func TestGitLockFailureRefLock(t *testing.T) {
	err := errors.New("fatal: Unable to create '/x/.git/refs/heads/main.lock': File exists.")
	if !IsGitLockFailure(err) {
		t.Fatalf("IsGitLockFailure(%q) = false, want true", err)
	}
	path, ok := GitLockPath(err.Error())
	if !ok || path != "/x/.git/refs/heads/main.lock" {
		t.Fatalf("GitLockPath(%q) = %q, %v, want %q, true", err, path, ok, "/x/.git/refs/heads/main.lock")
	}
}

func TestGitLockFailureUnrelatedError(t *testing.T) {
	err := errors.New("fatal: not a git repository (or any of the parent directories): .git")
	if IsGitLockFailure(err) {
		t.Fatalf("IsGitLockFailure(%q) = true, want false", err)
	}
	if _, ok := GitLockPath(err.Error()); ok {
		t.Fatalf("GitLockPath(%q) ok = true, want false", err)
	}
}

func TestGitLockFailureNilError(t *testing.T) {
	if IsGitLockFailure(nil) {
		t.Fatal("IsGitLockFailure(nil) = true, want false")
	}
}
