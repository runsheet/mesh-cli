//go:build !linux

package nodev1

import "projectmesh.io/mesh-cli/internal/supervisor"

// egressRuntime is the portable stub so the module still cross-compiles
// off linux: it finds no lease-egress processes at all, so the
// reconcile stops none (the node itself runs on linux; see
// egressproc_linux.go).
type egressRuntime struct{}

var _ supervisor.EgressRuntime = egressRuntime{}

func (egressRuntime) List() ([]supervisor.EgressProcess, error) { return nil, nil }
func (egressRuntime) Stop(pid int) error                        { return nil }
