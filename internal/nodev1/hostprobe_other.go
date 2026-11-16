//go:build !linux

package nodev1

import "fmt"

// ProcHostProbe exists so the module still cross-compiles off linux,
// which the node does not run on; it always refuses, so WatchHost
// never caches a reading from it.
type ProcHostProbe struct {
	Root string
}

func (p *ProcHostProbe) Read(leasePath, cachePath string) (MachineReading, error) {
	return MachineReading{}, fmt.Errorf("nodev1: host probe: not supported on this platform")
}
