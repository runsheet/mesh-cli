package nodev1

import (
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
// cause stalled, and reconciles the node's pods, job directories and
// relay grants against the actors every minute.

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
	return s
}
