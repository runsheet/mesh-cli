//go:build !linux

package nodev1

import "fmt"

// FSDiskProbe exists so the module still cross-compiles off linux,
// which the node does not run on; it always refuses, so diskGate and
// ReadBuildCacheConfig's default never read a free-space figure from
// it.
type FSDiskProbe struct{}

func (FSDiskProbe) FreeBytes(path string) (uint64, error) {
	return 0, fmt.Errorf("nodev1: disk free space: not supported on this platform")
}
