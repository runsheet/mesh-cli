package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// gitlockTestRun runs git in dir, failing the test on error.
func gitlockTestRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// gitlockTestRepo makes a fresh, committable git repo at t.TempDir(),
// the config writes that identify a checkout go against.
func gitlockTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitlockTestRun(t, dir, "init", "-q")
	gitlockTestRun(t, dir, "config", "user.email", "a@a.com")
	gitlockTestRun(t, dir, "config", "user.name", "a")
	return dir
}

func TestClearStaleGitLockUnrelatedMessage(t *testing.T) {
	called := false
	logf := func(string, ...any) { called = true }
	if ClearStaleGitLock("/x", "fatal: not a git repository (or any of the parent directories): .git", time.Minute, time.Now(), logf) {
		t.Fatal("ClearStaleGitLock(unrelated message) = true, want false")
	}
	if called {
		t.Fatal("ClearStaleGitLock logged on an unrelated message")
	}
}

// TestStaleConfigLockClearedAndRetried is this item's acceptance: a
// config.lock left behind by a process that is gone (its mtime two
// minutes old) is removed on the first failure, the write that failed
// on it is retried once and succeeds, and the removal is logged with
// the lock's path.
func TestStaleConfigLockClearedAndRetried(t *testing.T) {
	dir := gitlockTestRepo(t)
	lock := filepath.Join(dir, ".git", "config.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}

	var logs []string
	g := &Git{Logf: func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}}
	id := Identity{Name: "Mesh builder (mini)", Email: "mesh-builder+mini@runsheet.dev"}
	if err := g.Identify(context.Background(), dir, id); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("stale lock still on disk: stat err %v", err)
	}
	if got := gitlockTestRun(t, dir, "config", "--local", "--get", "user.name"); strings.TrimSpace(got) != id.Name {
		t.Fatalf("user.name = %q, want %q", got, id.Name)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "removed stale git lock") && strings.Contains(l, lock) {
			found = true
		}
	}
	if !found {
		t.Fatalf("log lines %v do not report removing %s", logs, lock)
	}
}

// TestFreshConfigLockIsNotClearedAndFailsAsBefore is this item's other
// half: a lock younger than StaleGitLockAge (five seconds old) is a
// write genuinely in progress, so it is left alone and the command
// fails as it always has — IsGitLockFailure true on the error, for a
// caller to release the claim as an infrastructure hold (ADR-024).
func TestFreshConfigLockIsNotClearedAndFailsAsBefore(t *testing.T) {
	dir := gitlockTestRepo(t)
	lock := filepath.Join(dir, ".git", "config.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	young := time.Now().Add(-5 * time.Second)
	if err := os.Chtimes(lock, young, young); err != nil {
		t.Fatal(err)
	}

	g := &Git{}
	id := Identity{Name: "Mesh builder (mini)", Email: "mesh-builder+mini@runsheet.dev"}
	err := g.Identify(context.Background(), dir, id)
	if err == nil {
		t.Fatal("Identify succeeded, want an error on the still-live lock")
	}
	if !IsGitLockFailure(err) {
		t.Fatalf("IsGitLockFailure(%v) = false, want true", err)
	}
	if _, statErr := os.Stat(lock); statErr != nil {
		t.Fatalf("young lock removed: stat err %v", statErr)
	}
}
