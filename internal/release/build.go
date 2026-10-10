package release

import "path/filepath"

// Target is one binary a release builds: the platform it is built for and
// the asset name it is published under (mesh-cli ADR-002's contract).
type Target struct {
	GOOS, GOARCH, Name string
}

// Targets is every binary a release builds, in publishing order: the
// three assets of release-tag.yml, in its order (ADR-017 section 4, step 1).
func Targets() []Target {
	return []Target{
		{GOOS: "linux", GOARCH: "amd64", Name: "mesh-linux-amd64"},
		{GOOS: "linux", GOARCH: "arm64", Name: "mesh-linux-arm64"},
		{GOOS: "darwin", GOARCH: "arm64", Name: "mesh-darwin-arm64"},
	}
}

// versionPkg is the package whose Version and Sha a release stamps.
const versionPkg = "projectmesh.io/mesh-cli/internal/version"

// Invocation is one command a release build runs: Args the argv, Env the
// variables set on top of the session's own.
type Invocation struct {
	Env  []string
	Args []string
}

// BuildPlan is the go build of every Target, in Targets' order, as
// release-tag.yml's "Build the three assets" runs it: CGO_ENABLED=0,
// -trimpath, -s -w, Version the tag and Sha the commit's first seven
// characters, each binary at outDir/<Target.Name> (ADR-017 section 4,
// step 1). It runs nothing.
func BuildPlan(version, sha, outDir string) []Invocation {
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	ldflags := "-s -w -X " + versionPkg + ".Version=" + version + " -X " + versionPkg + ".Sha=" + short
	var plan []Invocation
	for _, t := range Targets() {
		plan = append(plan, Invocation{
			Env: []string{"CGO_ENABLED=0", "GOOS=" + t.GOOS, "GOARCH=" + t.GOARCH},
			Args: []string{"go", "build", "-trimpath", "-ldflags", ldflags,
				"-o", filepath.Join(outDir, t.Name), "./cmd/mesh/"},
		})
	}
	return plan
}
