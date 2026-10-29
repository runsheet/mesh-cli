// Package supervisor watches a Mesh v1 node's job actors (ADR-044, Mesh
// v1 spec "The mesh-cli node": internal/supervisor). It does two things:
//
//   - Liveness by progress. A running lease whose session has shown no
//     progress (its progress_at, from the streaming session's output)
//     for QuietAfter is escalated first, through the lease's mailbox —
//     a stalled turn often moves on a bigger model — and, still quiet
//     QuietAfter after that, stopped with cause stalled. Mode warn only
//     logs both; mode cancel acts.
//   - A reconcile every minute: the node's pods with no actor, and pods
//     whose agent has ended with only a sidecar left, are removed; job
//     directories and relay grants no actor holds are removed; adopted
//     sessions are counted.
//
// The supervisor holds no state of a lease's own: what it knows of each
// it reads from the actors (Fleet) on every look.
package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// Mode is what the supervisor does about a quiet lease.
type Mode string

const (
	// Off runs no supervisor.
	Off Mode = "off"
	// Warn logs a quiet lease and what it would do, and does nothing.
	Warn Mode = "warn"
	// Cancel escalates a quiet lease, then stops it stalled.
	Cancel Mode = "cancel"
)

// ParseMode reads --supervisor: off, warn (or report) or cancel.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off":
		return Off, nil
	case "warn", "report":
		return Warn, nil
	case "cancel":
		return Cancel, nil
	}
	return Off, fmt.Errorf("supervisor: mode is off, warn or cancel, not %q", s)
}

// Defaults.
const (
	DefaultQuietAfter     = 15 * time.Minute
	DefaultLookEvery      = 15 * time.Second
	DefaultReconcileEvery = time.Minute
	// DefaultOrphanAge is how old a job directory with no actor must be
	// before the reconcile removes it: an actor makes its directory a
	// moment before the node lists it as live.
	DefaultOrphanAge = 2 * time.Minute
)

// Live is one live lease as its actor stands.
type Live struct {
	LeaseID string
	// Running is the actor's session running (not preparing, reporting
	// or releasing): only a running session can be quiet.
	Running bool
	// ProgressAt is the session's last progress.
	ProgressAt time.Time
	// EscalatedAt is when the supervisor escalated the lease for quiet;
	// zero when it has not since the lease's last progress.
	EscalatedAt time.Time
	// Pod is the lease's pod, "" for none.
	Pod string
	// Adopted is a session the node found running rather than started.
	Adopted bool
}

// Fleet is the node's actors as the supervisor sees and moves them.
type Fleet interface {
	// Live lists the live leases.
	Live() []Live
	// Escalate records that the supervisor escalated the lease now
	// (Live's EscalatedAt) and leaves a stall escalation in its mailbox;
	// false when there was none to leave (no mailbox, or the ladder at
	// its top), and the lease is stopped if it stays quiet all the same.
	Escalate(leaseID, why string) bool
	// Stall stops the lease with cause stalled.
	Stall(leaseID, why string)
	// Grants are the leases the node's relay holds a grant for.
	Grants() []string
	// CloseGrant closes a grant no actor holds.
	CloseGrant(leaseID string)
}

// Supervisor is the node's.
type Supervisor struct {
	Fleet Fleet
	Mode  Mode
	// QuietAfter is how long a running session may show no progress
	// before it is escalated, and again before it is stopped.
	QuietAfter time.Duration
	// LookEvery is how often liveness is looked at; ReconcileEvery how
	// often the node is reconciled.
	LookEvery      time.Duration
	ReconcileEvery time.Duration
	// Sandbox, when set, is the runtime whose pods are reconciled; only
	// pods labelled PodLabel are the node's.
	Sandbox  sandbox.Runtime
	PodLabel string
	// LeaseRoot holds the job directories, one per lease.
	LeaseRoot string
	// OrphanAge is how old an unheld job directory must be to go.
	OrphanAge time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Logf hears everything the supervisor sees and does.
	Logf func(format string, args ...any)
}

func (s *Supervisor) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *Supervisor) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Run looks and reconciles until ctx ends; it does nothing in mode off.
func (s *Supervisor) Run(ctx context.Context) {
	if s.Mode == Off || s.Mode == "" {
		return
	}
	look := time.NewTicker(or(s.LookEvery, DefaultLookEvery))
	defer look.Stop()
	rec := time.NewTicker(or(s.ReconcileEvery, DefaultReconcileEvery))
	defer rec.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-look.C:
			s.Look()
		case <-rec.C:
			s.Reconcile(ctx)
		}
	}
}

// Look is one pass of liveness over the live leases. In mode warn it
// logs once as each lease crosses a threshold — QuietAfter (it would be
// escalated) and twice that (it would be stopped) — and does nothing.
func (s *Supervisor) Look() {
	quiet := or(s.QuietAfter, DefaultQuietAfter)
	every := or(s.LookEvery, DefaultLookEvery)
	now := s.now()
	for _, l := range s.Fleet.Live() {
		if !l.Running || l.ProgressAt.IsZero() {
			continue
		}
		silent := now.Sub(l.ProgressAt)
		if silent < quiet {
			continue
		}
		said := silent.Round(time.Second)
		if s.Mode != Cancel {
			switch {
			case silent < quiet+every:
				s.logf("supervisor: lease %s: the session has shown no progress for %s (warn: it would be escalated)", l.LeaseID, said)
			case silent >= 2*quiet && silent < 2*quiet+every:
				s.logf("supervisor: lease %s: the session has shown no progress for %s (warn: it would be stopped stalled)", l.LeaseID, said)
			}
			continue
		}
		if l.EscalatedAt.IsZero() || l.EscalatedAt.Before(l.ProgressAt) {
			why := fmt.Sprintf("the session has shown no progress for %s", said)
			if s.Fleet.Escalate(l.LeaseID, why) {
				s.logf("supervisor: lease %s: %s; escalated through its mailbox", l.LeaseID, why)
			} else {
				s.logf("supervisor: lease %s: %s; it has nothing to escalate to, and is stopped if it stays quiet", l.LeaseID, why)
			}
			continue
		}
		if after := now.Sub(l.EscalatedAt); after >= quiet {
			why := fmt.Sprintf("the session showed no progress for %s, %s of it after escalating", said, after.Round(time.Second))
			s.logf("supervisor: lease %s: %s; stopping it stalled", l.LeaseID, why)
			s.Fleet.Stall(l.LeaseID, why)
		}
	}
}

// Reconciled is what one reconcile found and did.
type Reconciled struct {
	Actors, Adopted              int
	PodsNoActor, PodsSidecarOnly []string
	Workspaces, Grants           []string
	Errors                       []string
}

// Line is the reconcile's log line.
func (r Reconciled) Line() string {
	return fmt.Sprintf("supervisor: reconcile: %d actors (%d adopted); removed %d pods with no actor, %d with only a sidecar, %d job directories and %d grants no actor held",
		r.Actors, r.Adopted, len(r.PodsNoActor), len(r.PodsSidecarOnly), len(r.Workspaces), len(r.Grants))
}

// Reconcile is one reconcile of the node against its actors. In mode
// warn it removes nothing and logs what it would.
func (s *Supervisor) Reconcile(ctx context.Context) Reconciled {
	live := s.Fleet.Live()
	byLease := map[string]Live{}
	var out Reconciled
	for _, l := range live {
		byLease[l.LeaseID] = l
		out.Actors++
		if l.Adopted {
			out.Adopted++
		}
	}
	act := s.Mode == Cancel
	s.reconcilePods(ctx, byLease, act, &out)
	s.reconcileWorkspaces(byLease, act, &out)
	for _, id := range s.Fleet.Grants() {
		if _, ok := byLease[id]; ok {
			continue
		}
		out.Grants = append(out.Grants, id)
		if act {
			s.Fleet.CloseGrant(id)
		}
	}
	line := out.Line()
	if !act {
		line += " (warn: nothing removed)"
	}
	s.logf("%s", line)
	for _, e := range out.Errors {
		s.logf("supervisor: reconcile: %s", e)
	}
	return out
}

// reconcilePods removes the node's pods no actor holds, and those whose
// agent has ended leaving a sidecar running under an actor still
// running its session.
func (s *Supervisor) reconcilePods(ctx context.Context, live map[string]Live, act bool, out *Reconciled) {
	if s.Sandbox == nil || s.PodLabel == "" {
		return
	}
	pods, err := s.Sandbox.ListPods(ctx)
	if err != nil {
		out.Errors = append(out.Errors, "listing pods: "+err.Error())
		return
	}
	containers, err := s.Sandbox.ListContainers(ctx)
	if err != nil {
		out.Errors = append(out.Errors, "listing containers: "+err.Error())
		return
	}
	remove := func(name string) {
		if !act {
			return
		}
		if err := s.Sandbox.Remove(ctx, sandbox.Pod{Name: name}); err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("removing pod %s: %v", name, err))
		}
	}
	for _, p := range pods {
		lease, ours := p.Labels[s.PodLabel]
		if !ours {
			continue
		}
		l, held := live[lease]
		if !held {
			out.PodsNoActor = append(out.PodsNoActor, p.Name)
			remove(p.Name)
			continue
		}
		if !l.Running {
			continue
		}
		agent := sandbox.AgentName(sandbox.Pod{Name: p.Name})
		var agentSeen, agentRunning, sidecarRunning bool
		for _, c := range containers {
			if c.Pod != p.Name || c.Infra {
				continue
			}
			if c.Name == agent {
				agentSeen, agentRunning = true, c.Running
				continue
			}
			if c.Running {
				sidecarRunning = true
			}
		}
		if agentSeen && !agentRunning && sidecarRunning {
			out.PodsSidecarOnly = append(out.PodsSidecarOnly, p.Name)
			remove(p.Name)
		}
	}
}

// reconcileWorkspaces removes the job directories no actor holds, once
// they are OrphanAge old.
func (s *Supervisor) reconcileWorkspaces(live map[string]Live, act bool, out *Reconciled) {
	if s.LeaseRoot == "" {
		return
	}
	ents, err := os.ReadDir(s.LeaseRoot)
	if err != nil {
		return
	}
	age := or(s.OrphanAge, DefaultOrphanAge)
	now := s.now()
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, ok := live[e.Name()]; ok {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < age {
			continue
		}
		dir := filepath.Join(s.LeaseRoot, e.Name())
		out.Workspaces = append(out.Workspaces, dir)
		if act {
			if err := os.RemoveAll(dir); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("removing %s: %v", dir, err))
			}
		}
	}
}
