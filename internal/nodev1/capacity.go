package nodev1

import (
	"context"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// The node's capacity through the scheduler (ADR-044, Mesh v1 PR 3.1):
// with Runtime.Scheduler set (`mesh node run --actors-scheduler`) every
// lease holds a scheduler lease for its brief's resources, one job slot,
// one session of its model account and a shared hold on the build
// cache, from its take to its end; the node reports what no lease holds
// as `free` on every take, takes again only while a job slot is free,
// and runs its leases side by side. Without it the node runs one lease
// at a time and reports its whole capacity, as wave 0 did.

// roomWait bounds how long a lease Mesh gave the node waits for the
// scheduler's room — Mesh's ledger and the node's can disagree for a
// moment (a trim holding the cache, a lease that has not yet lapsed) —
// before the node hands it back cannot_run.
const roomWait = 30 * time.Second

// freeOf is the scheduler's free capacity as Mesh's take reads it.
func freeOf(s *scheduler.Scheduler) *meshapi.Capacity {
	f := s.Free()
	return &meshapi.Capacity{CPUs: f.CPUs, MemGB: f.MemGB, Slots: f.Slots}
}

// askOf is what a lease holds of the node: its brief's resources, a job
// slot, a session of its model account and the build cache shared, for
// a TTL of three renewals.
func askOf(b *meshapi.Brief, lease *meshapi.Lease) scheduler.Ask {
	every := time.Duration(lease.RenewEveryS) * time.Second
	if every <= 0 {
		every = 30 * time.Second
	}
	return scheduler.Ask{
		Owner: lease.LeaseID, CPUs: b.Resources.CPUs, MemGB: b.Resources.MemGB, Slot: true,
		Account: b.CredentialRef, Cache: scheduler.CacheShared, TTL: 3 * every,
	}
}

// holdCapacity takes the lease's room in the scheduler, waiting at most
// roomWait; nil and no error when the node has no scheduler.
func (r *Runtime) holdCapacity(ctx context.Context, b *meshapi.Brief, lease *meshapi.Lease) (*scheduler.Lease, error) {
	if r.Scheduler == nil {
		return nil, nil
	}
	ask := askOf(b, lease)
	if l, err := r.Scheduler.TryTake(ask); err == nil {
		return l, nil
	}
	wctx, cancel := context.WithTimeout(ctx, roomWait)
	defer cancel()
	return r.Scheduler.Take(wctx, ask)
}

// waitForSlot waits until the scheduler has a job slot free, so the node
// takes no lease it has no slot for; at once without a scheduler.
func (r *Runtime) waitForSlot(ctx context.Context) {
	if r.Scheduler == nil {
		return
	}
	for ctx.Err() == nil {
		ch := r.Scheduler.Changed()
		if r.Scheduler.Total().Slots <= 0 || r.Scheduler.Free().Slots > 0 {
			return
		}
		// a lease that lapses frees its slot without a change to wake on
		t := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
		case <-ch:
		case <-t.C:
		}
		t.Stop()
	}
}
