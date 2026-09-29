package version

import (
	"runtime/debug"
	"sync"
	"testing"
)

// set builds with the given ldflags values and vcs stamp.
func set(t *testing.T, v, c string, settings ...debug.BuildSetting) {
	t.Helper()
	oldV, oldC, oldRead, oldRun := Version, Sha, readBuildInfo, goRunCommit
	t.Cleanup(func() {
		Version, Sha, readBuildInfo, goRunCommit = oldV, oldC, oldRead, oldRun
		stampOnce, stamp = sync.Once{}, ""
	})
	Version, Sha = v, c
	goRunCommit = func() string { return "" }
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: settings}, true
	}
	stampOnce, stamp = sync.Once{}, ""
}

func rev(sha string) debug.BuildSetting { return debug.BuildSetting{Key: "vcs.revision", Value: sha} }

const sha = "0895d40c1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b"

func TestResolvedTagWins(t *testing.T) {
	set(t, "v0.2.188", "abc1234", rev(sha))
	if got := Resolved(); got != "v0.2.188" {
		t.Fatalf("Resolved() = %q, want v0.2.188", got)
	}
	if got := Long(); got != "mesh v0.2.188 (commit abc1234)" {
		t.Fatalf("Long() = %q", got)
	}
}

func TestResolvedCommitWhenDev(t *testing.T) {
	set(t, "dev", "", rev(sha))
	if got := Resolved(); got != "0895d40" {
		t.Fatalf("Resolved() = %q, want the short sha 0895d40", got)
	}
	if got := Long(); got != "mesh 0895d40 (dev build)" {
		t.Fatalf("Long() = %q", got)
	}
}

func TestResolvedLdflagsCommitWhenDev(t *testing.T) {
	set(t, "dev", "abc1234", rev(sha))
	if got := Resolved(); got != "abc1234" {
		t.Fatalf("Resolved() = %q, want abc1234 (the ldflag beats the stamp)", got)
	}
}

func TestResolvedDevWhenNeither(t *testing.T) {
	set(t, "dev", "")
	if got := Resolved(); got != "dev" {
		t.Fatalf("Resolved() = %q, want dev", got)
	}
	if got := Commit(); got != "" {
		t.Fatalf("Commit() = %q, want empty", got)
	}
	if got := Long(); got != "mesh dev" {
		t.Fatalf("Long() = %q", got)
	}
}

// a binary with no build info at all (readBuildInfo false) is dev
func TestResolvedDevWithoutBuildInfo(t *testing.T) {
	set(t, "dev", "")
	readBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	if got := Resolved(); got != "dev" {
		t.Fatalf("Resolved() = %q, want dev", got)
	}
	if got := Commit(); got != "" {
		t.Fatalf("Commit() = %q, want empty", got)
	}
}

// the tag stamped by the Version ldflag alone: the sha comes from the
// toolchain's vcs stamp, and both are said
func TestResolvedTagStampedCommitFromVCS(t *testing.T) {
	set(t, "v0.2.189", "", rev(sha))
	if got := Resolved(); got != "v0.2.189" {
		t.Fatalf("Resolved() = %q, want v0.2.189", got)
	}
	if got := Commit(); got != "0895d40" {
		t.Fatalf("Commit() = %q, want 0895d40", got)
	}
	if got := Long(); got != "mesh v0.2.189 (commit 0895d40)" {
		t.Fatalf("Long() = %q", got)
	}
}

// a `go run` binary carries no vcs stamp: its commit is the checkout's
func TestCommitGoRunAsksTheCheckout(t *testing.T) {
	set(t, "dev", "")
	goRunCommit = func() string { return "bd6b311" }
	if got := Resolved(); got != "bd6b311" {
		t.Fatalf("Resolved() = %q, want bd6b311", got)
	}
	if got := Long(); got != "mesh bd6b311 (dev build)" {
		t.Fatalf("Long() = %q", got)
	}
}

// the go run fallback is only for a binary under go-build: the test
// binary is not one, so it asks nothing
func TestGitHeadOnlyForGoRun(t *testing.T) {
	if got := gitHead(); got != "" {
		t.Fatalf("gitHead() = %q from a binary go run did not build", got)
	}
}

func TestCommitDirty(t *testing.T) {
	set(t, "dev", "", rev(sha), debug.BuildSetting{Key: "vcs.modified", Value: "true"})
	if got := Commit(); got != "0895d40-dirty" {
		t.Fatalf("Commit() = %q, want 0895d40-dirty", got)
	}
	if got := Resolved(); got != "0895d40-dirty" {
		t.Fatalf("Resolved() = %q", got)
	}
}

func TestCommitClean(t *testing.T) {
	set(t, "dev", "", rev(sha), debug.BuildSetting{Key: "vcs.modified", Value: "false"})
	if got := Commit(); got != "0895d40" {
		t.Fatalf("Commit() = %q, want 0895d40", got)
	}
}

func TestRelease(t *testing.T) {
	for v, want := range map[string]bool{"v0.2.188": true, "v1.0.0-rc1": true, "dev": false, "": false, "0895d40": false, "0895d40-dirty": false, "v": false, "vabc": false} {
		if got := Release(v); got != want {
			t.Errorf("Release(%q) = %v, want %v", v, got, want)
		}
	}
}
