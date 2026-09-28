// Package version is what this binary says it is: the release tag it was
// built from, or the commit when it was not (mesh-specs ADR 013, trd §1
// Node `version` and `commit`; mesh-cli adr-002's amendment).
package version

import (
	"runtime/debug"
	"sync"
)

// Version is the build-time version string. Default "dev" for local
// builds; CI overrides via -ldflags "-X projectmesh.io/mesh-cli/internal/version.Version=vX.Y.Z".
var Version = "dev"

// Commit is the short git sha the binary was built from, set by
// release-tag.yml via -ldflags "-X projectmesh.io/mesh-cli/internal/version.Commit=<sha>".
// Empty leaves BuildCommit to the Go toolchain's vcs stamp.
var Commit = ""

// readBuildInfo is debug.ReadBuildInfo; tests replace it.
var readBuildInfo = debug.ReadBuildInfo

var (
	stampOnce sync.Once
	stamp     string
)

// shortSHA is how much of vcs.revision BuildCommit keeps.
const shortSHA = 7

// BuildCommit is the commit this binary was built from: Commit when the
// build set it, else the toolchain's vcs.revision cut to a short sha with
// "-dirty" when vcs.modified, else empty (a build outside a checkout, or
// with -buildvcs=false).
func BuildCommit() string {
	if Commit != "" {
		return Commit
	}
	stampOnce.Do(func() { stamp = vcsCommit() })
	return stamp
}

// vcsCommit reads the vcs stamp from the build info.
func vcsCommit() string {
	info, ok := readBuildInfo()
	if !ok || info == nil {
		return ""
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > shortSHA {
		rev = rev[:shortSHA]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

// String is what the node reports itself as, to the platform and the
// operator: the release tag when the binary was built from one, else its
// commit, else "dev".
func String() string {
	if Version != "" && Version != "dev" {
		return Version
	}
	if c := BuildCommit(); c != "" {
		return c
	}
	return "dev"
}

// Release says whether v names a release (a tag, vX.Y.Z…) rather than a
// dev build's commit or "dev": only a release follows the platform's
// desired version (self-update).
func Release(v string) bool {
	return len(v) > 1 && v[0] == 'v' && v[1] >= '0' && v[1] <= '9'
}

// Long is `mesh version`'s line: "mesh v0.2.188 (commit 0895d40)", or
// "mesh 0895d40 (dev build)" for a build from no tag.
func Long() string {
	v, c := String(), BuildCommit()
	switch {
	case v == "dev":
		return "mesh dev"
	case v == c:
		return "mesh " + v + " (dev build)"
	case c != "":
		return "mesh " + v + " (commit " + c + ")"
	}
	return "mesh " + v
}
