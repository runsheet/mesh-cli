package release

import (
	"fmt"
	"os"
	"path/filepath"
)

// VersionName is the file a release shipped through runsheet lists in
// SHA256SUMS whose bytes are the version alone, no newline (mesh-cli ADR
// 002 amendment 2026-11-15): the certificate our own Sigstore issues names
// the deploy job, not the tag, so the signed sums name the version. It is
// internal/selfupdate's VersionAsset.
const VersionName = "mesh-version"

// WriteVersion writes dir/mesh-version holding version.
func WriteVersion(dir, version string) error {
	if version == "" {
		return fmt.Errorf("version: empty")
	}
	return os.WriteFile(filepath.Join(dir, VersionName), []byte(version), 0o644)
}

// Artefacts are every file a release built by Build ships, besides
// SHA256SUMS and its bundle: the binaries in Targets' order, then
// mesh-version.
func Artefacts() []string {
	var out []string
	for _, t := range Targets() {
		out = append(out, t.Name)
	}
	return append(out, VersionName)
}
