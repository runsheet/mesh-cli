package release

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
