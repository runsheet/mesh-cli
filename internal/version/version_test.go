package version

import (
	"runtime/debug"
	"sync"
	"testing"
)

// set builds with the given ldflags values and vcs stamp.
func set(t *testing.T, v, c string, settings ...debug.BuildSetting) {
	t.Helper()
	oldV, oldC, oldRead := Version, Commit, readBuildInfo
	t.Cleanup(func() {
		Version, Commit, readBuildInfo = oldV, oldC, oldRead
		stampOnce, stamp = sync.Once{}, ""
	})
	Version, Commit = v, c
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: settings}, true
	}
	stampOnce, stamp = sync.Once{}, ""
}

func rev(sha string) debug.BuildSetting { return debug.BuildSetting{Key: "vcs.revision", Value: sha} }

const sha = "0895d40c1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b"

func TestStringTagWins(t *testing.T) {
	set(t, "v0.2.188", "abc1234", rev(sha))
	if got := String(); got != "v0.2.188" {
		t.Fatalf("String() = %q, want v0.2.188", got)
	}
	if got := Long(); got != "mesh v0.2.188 (commit abc1234)" {
		t.Fatalf("Long() = %q", got)
	}
}

func TestStringCommitWhenDev(t *testing.T) {
	set(t, "dev", "", rev(sha))
	if got := String(); got != "0895d40" {
		t.Fatalf("String() = %q, want the short sha 0895d40", got)
	}
	if got := Long(); got != "mesh 0895d40 (dev build)" {
		t.Fatalf("Long() = %q", got)
	}
}

func TestStringLdflagsCommitWhenDev(t *testing.T) {
	set(t, "dev", "abc1234", rev(sha))
	if got := String(); got != "abc1234" {
		t.Fatalf("String() = %q, want abc1234 (the ldflag beats the stamp)", got)
	}
}

func TestStringDevWhenNeither(t *testing.T) {
	set(t, "dev", "")
	if got := String(); got != "dev" {
		t.Fatalf("String() = %q, want dev", got)
	}
	if got := BuildCommit(); got != "" {
		t.Fatalf("BuildCommit() = %q, want empty", got)
	}
	if got := Long(); got != "mesh dev" {
		t.Fatalf("Long() = %q", got)
	}
}

func TestBuildCommitDirty(t *testing.T) {
	set(t, "dev", "", rev(sha), debug.BuildSetting{Key: "vcs.modified", Value: "true"})
	if got := BuildCommit(); got != "0895d40-dirty" {
		t.Fatalf("BuildCommit() = %q, want 0895d40-dirty", got)
	}
	if got := String(); got != "0895d40-dirty" {
		t.Fatalf("String() = %q", got)
	}
}

func TestBuildCommitClean(t *testing.T) {
	set(t, "dev", "", rev(sha), debug.BuildSetting{Key: "vcs.modified", Value: "false"})
	if got := BuildCommit(); got != "0895d40" {
		t.Fatalf("BuildCommit() = %q, want 0895d40", got)
	}
}

func TestRelease(t *testing.T) {
	for v, want := range map[string]bool{"v0.2.188": true, "v1.0.0-rc1": true, "dev": false, "": false, "0895d40": false, "0895d40-dirty": false, "v": false, "vabc": false} {
		if got := Release(v); got != want {
			t.Errorf("Release(%q) = %v, want %v", v, got, want)
		}
	}
}
