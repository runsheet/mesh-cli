package nodev1

import (
	"os"
	"time"
)

// UpgradeExit is what UpgradeSignal calls once every live lease's
// hand-off is written and every hand-back it made is reported to Mesh
// (or its wait is up): os.Exit, so the process ends at once for its
// service manager (or the next `mesh node run --api v1`) to start the
// binary `mesh node upgrade --api v1` already swapped onto disk
// (adr-009's restart, carried to a v1 node by adr-044's hand-off: every
// lease left running is untouched here, so nothing waits on one).
// Swappable so a test can observe the call without ending the test
// binary.
var UpgradeExit = os.Exit

// handBackWait bounds how long UpgradeSignal waits for Upgrade's
// hand-backs to be reported before it exits: twice reportTimeout, the
// bound every report to Mesh is itself held to, so a hand-back whose
// report takes the whole of reportTimeout to answer is never raced by
// the exit that follows it.
const handBackWait = 2 * reportTimeout

// UpgradeSignal is the node's own reaction to its restart signal
// (`mesh node upgrade --api v1`, internal/commands/nodeupgrade.go,
// wired from internal/commands/noderun.go the way its drain signal
// calls Drain): it writes every live lease's hand-off, hands back the
// ones a hand-off can never pick back up (Upgrade), waits -- bounded --
// for Mesh to hear of those hand-backs, and exits the process -- never
// by cancelling the root context a lease's own context descends from,
// which would end every lease an upgrade means to leave running. A
// hand-back os.Exit cut off mid-report would leave its lease's
// actor.json Cancelling on disk forever, for no later process to ever
// adopt or release (adr-044's hand-off adopts a hand-off, not a
// hand-back); the wait is what keeps that from happening.
func (r *Runtime) UpgradeSignal(why string) {
	handedBack := r.Upgrade(why)
	r.awaitReported(handedBack, handBackWait)
	UpgradeExit(0)
}

// awaitReported blocks until none of ids is still in flight -- gone
// from r.leases, which runActor deletes only after its actor has
// reported its end to Mesh (actorlease.go's runActor: a.Reported()
// runs, then r.leases.Delete(id), before the goroutine returns) -- or
// until wait elapses, whichever comes first. With no ids it returns at
// once: the ordinary upgrade, which only writes hand-offs, never waits.
func (r *Runtime) awaitReported(ids []string, wait time.Duration) {
	if len(ids) == 0 {
		return
	}
	deadline := time.Now().Add(wait)
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		live := 0
		for _, id := range ids {
			if _, ok := r.leases.Get(id); ok {
				live++
			}
		}
		if live == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			r.logf("nodev1: %d hand-back(s) still unreported after %s; exiting anyway", live, wait)
			return
		}
		<-t.C
	}
}
