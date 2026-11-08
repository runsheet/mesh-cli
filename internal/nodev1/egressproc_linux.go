//go:build linux

package nodev1

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"

	"projectmesh.io/mesh-cli/internal/supervisor"
)

// egressRuntime implements supervisor.EgressRuntime by reading /proc:
// the node runs on linux (the mini runs linux/amd64), and a lease's
// egress process has no other registry of its own once its actor is
// gone (ADR-051 decision 3) -- its argv is all that is left to find it
// by, the same argv startEgressProcess (relaypod.go) gave it.
type egressRuntime struct{}

var _ supervisor.EgressRuntime = egressRuntime{}

// List finds every running `mesh lease-egress --control ...` process on
// this host, named only by pid: the reconcile cross-checks each against
// the live leases' own EgressPID, never this process's argv further.
func (egressRuntime) List() ([]supervisor.EgressProcess, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []supervisor.EgressProcess
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if isLeaseEgress(pid) {
			out = append(out, supervisor.EgressProcess{PID: pid})
		}
	}
	return out, nil
}

// Stop ends a lease-egress process the reconcile found no live lease
// naming, the same bare-pid stop releaseLeftovers uses for one an
// actor.json leftover names (decision 5).
func (egressRuntime) Stop(pid int) error {
	return stopLeftoverEgress(pid)
}

// isLeaseEgress reads pid's cmdline and says whether it is a
// `mesh lease-egress --control <path>` process (relaypod.go's
// startEgressProcess); a pid already gone, or one whose cmdline this
// process cannot read (another user's, a kernel thread), is not one.
func isLeaseEgress(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || len(data) == 0 {
		return false
	}
	args := bytes.Split(bytes.TrimRight(data, "\x00"), []byte{0})
	foundSub, foundControl := false, false
	for _, a := range args {
		switch string(a) {
		case "lease-egress":
			foundSub = true
		case "--control":
			foundControl = true
		}
	}
	return foundSub && foundControl
}
