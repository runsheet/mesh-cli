package nodev1

import "os"

// UpgradeExit is what UpgradeSignal calls once every live lease's
// hand-off is written: os.Exit, so the process ends at once for its
// service manager (or the next `mesh node run --api v1`) to start the
// binary `mesh node upgrade --api v1` already swapped onto disk
// (adr-009's restart, carried to a v1 node by adr-044's hand-off: every
// lease is left running untouched, so nothing here waits on one).
// Swappable so a test can observe the call without ending the test
// binary.
var UpgradeExit = os.Exit

// UpgradeSignal is the node's own reaction to its restart signal
// (`mesh node upgrade --api v1`, internal/commands/nodeupgrade.go,
// wired from internal/commands/noderun.go the way its drain signal
// calls Drain): it writes every live lease's hand-off (Upgrade) and
// exits the process -- never by cancelling the root context a lease's
// own context descends from, which would end every lease an upgrade
// means to leave running.
func (r *Runtime) UpgradeSignal(why string) {
	r.Upgrade(why)
	UpgradeExit(0)
}
