package nodev1

import (
	"context"
	"errors"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/escalate"
	"projectmesh.io/mesh-cli/internal/supervisor"
)

// The supervisor (ADR-044, Mesh v1 PR 3.3; `mesh node run --supervisor
// warn|cancel`, with --actors): it reads the node's job actors through
// fleet — each running lease's progress_at and when it was escalated —
// escalates a quiet session through its mailbox, then stops it with
// cause stalled, and reconciles the node's pods, job directories,
// relay grants and lease-egress processes against the actors every
// minute (ADR-051 decision 3).

// errStalled is the supervisor stopping a lease whose session stayed
// quiet past its escalation.
var errStalled = errors.New("the supervisor stopped the lease as stalled")

// fleet is the runtime's actors as the supervisor sees them.
type fleet struct{ r *Runtime }

var _ supervisor.Fleet = fleet{}

// Live implements supervisor.Fleet: every lease run by a job actor.
func (f fleet) Live() []supervisor.Live {
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	all := f.r.leases.All()
	out := make([]supervisor.Live, 0, len(all))
	for _, e := range all {
		id, l := e.ID, e.Value
		if l.actor == nil {
			continue
		}
		out = append(out, supervisor.Live{
			LeaseID: id, Running: l.actor.State() == actor.Running,
			ProgressAt: l.progressAt, EscalatedAt: l.escalatedAt, Pod: l.pod,
			EgressPID: l.egressPID,
		})
	}
	return out
}

// Escalate implements supervisor.Fleet: a stall escalation in the
// lease's mailbox, which the session's loop takes up the kind's ladder
// when the kind escalates on stall.
func (f fleet) Escalate(id, why string) bool {
	f.r.mu.Lock()
	l := f.r.lease(id)
	if l != nil {
		l.escalatedAt = time.Now()
	}
	f.r.mu.Unlock()
	if l == nil || l.mailbox == nil {
		return false
	}
	return l.mailbox.Post(Message{Kind: MsgEscalate, On: escalate.OnStall, Text: clip(why, 500)}) == nil
}

// Stall implements supervisor.Fleet: the lease's actor is cancelled and
// the lease handed back stalled.
func (f fleet) Stall(id, why string) {
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	l := f.r.lease(id)
	if l == nil {
		return
	}
	if l.cancelled == "" {
		l.cancelled = why
	}
	if l.actor != nil {
		l.actor.Cancel(why)
	}
	l.cancel(errStalled)
}

// Grants implements supervisor.Fleet.
func (f fleet) Grants() []string { return f.r.relay.Leases() }

// CloseGrant implements supervisor.Fleet.
func (f fleet) CloseGrant(id string) {
	if g := f.r.relay.Grant(id); g != nil {
		if err := g.Close(); err != nil {
			f.r.logf("nodev1: closing lease %s's leftover grant: %v", id, err)
		}
	}
}

// supervisor is the node's supervisor, nil when SupervisorMode is off or
// the node runs no actors.
func (r *Runtime) supervisor() *supervisor.Supervisor {
	if !r.Actors || r.SupervisorMode == "" || r.SupervisorMode == supervisor.Off {
		return nil
	}
	s := &supervisor.Supervisor{
		Fleet: fleet{r}, Mode: r.SupervisorMode, QuietAfter: r.QuietAfter,
		LookEvery: r.SuperviseEvery, ReconcileEvery: r.ReconcileEvery,
		PodLabel: PodLeaseLabel, LeaseRoot: r.leaseDir(""), Logf: r.Logf,
	}
	if r.Sandbox != nil {
		s.Sandbox = r.Sandbox
	}
	s.Egress = egressRuntime{}
	return s
}

// WatchSupervisor is the node.yaml supervisor-mode watcher (ADR-044
// amendment, 2026-11-09): it rereads node.yaml's `v1.supervisor` with
// read every poll, and at once whenever reload fires, calling the
// running supervisor's SetMode whenever what it read differs from the
// mode last applied — so an operator who edits node.yaml and signals
// the process (wired to SIGHUP by `mesh node run`, the same signal
// WatchCapacity answers) changes the supervisor's mode without a
// restart: the very next Look or Reconcile reads it
// (supervisor.Supervisor.SetMode). A read that errors or finds no key
// changes nothing. With no supervisor running at start (SupervisorMode
// was off, or Actors is off) r.sup is nil and the watcher does
// nothing: a supervisor cannot be switched on from off this way, only
// retuned while it already runs.
func (r *Runtime) WatchSupervisor(ctx context.Context, read func() (supervisor.Mode, bool, error), poll time.Duration, reload <-chan struct{}) {
	if read == nil || r.sup == nil {
		return
	}
	if poll <= 0 {
		poll = time.Minute
	}
	last := r.SupervisorMode
	t := time.NewTicker(poll)
	defer t.Stop()
	apply := func() {
		m, ok, err := read()
		if err != nil {
			r.logf("nodev1: supervisor: node.yaml: %v", err)
			return
		}
		if !ok || m == last {
			return
		}
		last = m
		r.sup.SetMode(m)
		r.logf("nodev1: supervisor: mode now %s", m)
	}
	apply()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			apply()
		case <-reload:
			apply()
		}
	}
}
