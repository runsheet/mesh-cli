// Package release is the release unit a node runs for a release-worthy merge
// to runsheet/mesh-cli (mesh-specs ADR 017): it builds the binaries, sums and
// signs them under the platform-held identity, publishes the release and the
// public mirror, and attests what it shipped.
package release
