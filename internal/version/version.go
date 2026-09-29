// Package version is what this binary says it is: the release tag it was
// built from, or the commit when it was not (mesh-specs ADR 013, trd §1
// Node `version` and `commit`; mesh-cli adr-002's amendment).
package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

// Version is the build-time version string. Default "dev" for local
// builds; CI overrides via -ldflags "-X projectmesh.io/mesh-cli/internal/version.Version=vX.Y.Z".
var Version = "dev"

// Sha is the short git sha the binary was built from, set by
// release-tag.yml via -ldflags "-X projectmesh.io/mesh-cli/internal/version.Sha=<sha>".
// Empty leaves Commit to the Go toolchain's vcs stamp.
var Sha = ""

// readBuildInfo is debug.ReadBuildInfo; tests replace it.
var readBuildInfo = debug.ReadBuildInfo

// goRunCommit is the commit of a `go run` binary, which the toolchain
// never stamps (-buildvcs auto leaves go run out); tests replace it.
var goRunCommit = gitHead

var (
	stampOnce sync.Once
	stamp     string
)

// shortSHA is how much of vcs.revision Commit keeps.
const shortSHA = 7

// Commit is the commit this binary was built from: Sha when the
// build set it, else the toolchain's vcs.revision cut to a short sha with
// "-dirty" when vcs.modified, else — for a `go run` binary only — the
// checkout it runs in, else empty (a build outside a checkout, or with
// -buildvcs=false).
func Commit() string {
	if Sha != "" {
		return Sha
	}
	stampOnce.Do(func() { stamp = vcsCommit() })
	return stamp
}

// vcsCommit reads the vcs stamp from the build info.
func vcsCommit() string {
	info, ok := readBuildInfo()
	if !ok || info == nil {
		return goRunCommit()
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
		return goRunCommit()
	}
	if len(rev) > shortSHA {
		rev = rev[:shortSHA]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

// gitHead is the working directory's HEAD, short, with "-dirty" when the
// tree has changes — asked only of a binary `go run` built (its
// executable under a go-build temporary directory), which runs from the
// checkout it was built from. Any other binary without a stamp has no
// commit: the directory it runs in says nothing about it.
func gitHead() string {
	exe, err := os.Executable()
	if err != nil || !strings.HasPrefix(filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(exe)))), "go-build") {
		return ""
	}
	out, err := exec.Command("git", "rev-parse", "--short="+strconv.Itoa(shortSHA), "HEAD").Output()
	rev := strings.TrimSpace(string(out))
	if err != nil || rev == "" {
		return ""
	}
	if st, err := exec.Command("git", "status", "--porcelain").Output(); err == nil && len(strings.TrimSpace(string(st))) > 0 {
		rev += "-dirty"
	}
	return rev
}

// Resolved is what the node reports itself as, to the platform and the
// operator: the release tag when the binary was built from one, else its
// commit, else "dev".
func Resolved() string {
	if Version != "" && Version != "dev" {
		return Version
	}
	if c := Commit(); c != "" {
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
	v, c := Resolved(), Commit()
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
