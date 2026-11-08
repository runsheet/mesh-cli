// Package supervisor watches a Mesh v1 node's job actors (ADR-044, Mesh
// v1 spec "The mesh-cli node": internal/supervisor). It does two things:
//
//   - Liveness by progress. A running lease whose session has shown no
//     progress (its progress_at, from the streaming session's output)
//     for QuietAfter is escalated first, through the lease's mailbox —
//     a stalled turn often moves on a bigger model — and, still quiet
//     QuietAfter after that, stopped with cause stalled. Mode warn only
//     logs both; mode cancel acts; mode tidy never escalates or stalls
//     a lease at all (it only logs, as warn does) — tidy's whole job is
//     the reconcile below.
//   - A reconcile every minute: the node's pods with no actor, and pods
//     whose agent has ended with only a sidecar left, are removed; a
//     lease-egress process no actor and no hand-off names is stopped
//     (ADR-051 decision 3); job directories and relay grants no actor
//     holds are removed; adopted sessions are counted.
//
// The supervisor holds no state of a lease's own: what it knows of each
// it reads from the actors (Fleet) on every look.
package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// handoffFileName is "handoff.json", the name nodev1's own handoffFile
// constant gives a lease's hand-off state under its job directory: a
// directory naming one is a deliberate hand-off, not a leftover, so its
// egress process is left alone (ADR-051 decision 3). Named here too
// since this package cannot import nodev1 (nodev1 imports supervisor).
const handoffFileName = "handoff.json"

// Mode is what the supervisor does about a quiet lease.
type Mode string

const (
	// Off runs no supervisor.
	Off Mode = "off"
	// Warn logs a quiet lease and what it would do, and does nothing.
	Warn Mode = "warn"
	// Cancel escalates a quiet lease, then stops it stalled.
	Cancel Mode = "cancel"
	// Tidy reconciles the node against its actors the same way Cancel
	// does — removing pods, job directories, grants and lease-egress
	// processes no actor holds — but never escalates or stalls a lease
	// for quiet: it only logs a quiet lease, as Warn does.
	Tidy Mode = "tidy"
)

// ParseMode reads --supervisor: off, warn (or report), cancel or tidy.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off":
		return Off, nil
	case "warn", "report":
		return Warn, nil
	case "cancel":
		return Cancel, nil
	case "tidy":
		return Tidy, nil
	}
	return Off, fmt.Errorf("supervisor: mode is off, warn, cancel or tidy, not %q", s)
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
	// EgressPID is the lease's egress process (ADR-051 decision 1), 0
	// for a lease with none (in-process relay, or no sidecar at all).
	EgressPID int
}

// EgressProcess is a running `mesh lease-egress` the reconcile found on
// the host, named only by its pid (ADR-051 decision 3).
type EgressProcess struct {
	PID int
}

// EgressRuntime lists and stops the node's own lease-egress processes.
// Nil runs no egress reconcile at all (a platform with no /proc, or a
// node not built with one).
type EgressRuntime interface {
	List() ([]EgressProcess, error)
	Stop(pid int) error
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
	// Mode is read by Run, Look and Reconcile through mode(), under mu,
	// so a SetMode call from another goroutine takes effect on the next
	// one of them, even mid-Run; set it directly only before the
	// Supervisor's first use.
	Mode Mode
	mu   sync.Mutex
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
	// Egress, when set, lists and stops the node's own lease-egress
	// processes (ADR-051 decision 3): one no live lease's EgressPID and
	// no hand-off names — listed, or named by a leftover lease's
	// actor.json under LeaseRoot — is stopped the way a pod no actor
	// holds is removed.
	Egress EgressRuntime
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

// mode is the Supervisor's current Mode, safe alongside a concurrent
// SetMode.
func (s *Supervisor) mode() Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Mode
}

// SetMode changes the supervisor's mode. It takes effect at once: the
// next Look or Reconcile — including one Run is about to make, or one
// already running concurrently with this call — reads the new mode.
func (s *Supervisor) SetMode(m Mode) {
	s.mu.Lock()
	s.Mode = m
	s.mu.Unlock()
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Run looks and reconciles until ctx ends; it does nothing in mode off.
func (s *Supervisor) Run(ctx context.Context) {
	if m := s.mode(); m == Off || m == "" {
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

// Look is one pass of liveness over the live leases. In mode warn or
// tidy it logs once as each lease crosses a threshold — QuietAfter (it
// would be escalated) and twice that (it would be stopped) — and does
// nothing: only mode cancel escalates or stalls a lease for quiet.
func (s *Supervisor) Look() {
	mode := s.mode()
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
		if mode != Cancel {
			switch {
			case silent < quiet+every:
				s.logf("supervisor: lease %s: the session has shown no progress for %s (%s: it would be escalated)", l.LeaseID, said, mode)
			case silent >= 2*quiet && silent < 2*quiet+every:
				s.logf("supervisor: lease %s: the session has shown no progress for %s (%s: it would be stopped stalled)", l.LeaseID, said, mode)
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
	// EgressNoActor is the lease-egress processes the reconcile found no
	// live lease and no hand-off naming (ADR-051 decision 3), by pid.
	EgressNoActor []int
	Errors        []string
}

// Line is the reconcile's log line.
func (r Reconciled) Line() string {
	return fmt.Sprintf("supervisor: reconcile: %d actors (%d adopted); removed %d pods with no actor, %d with only a sidecar, %d job directories and %d grants no actor held; stopped %d egress process(es) with no actor and no hand-off",
		r.Actors, r.Adopted, len(r.PodsNoActor), len(r.PodsSidecarOnly), len(r.Workspaces), len(r.Grants), len(r.EgressNoActor))
}

// Reconcile is one reconcile of the node against its actors. In mode
// warn it removes nothing and logs what it would; modes cancel and
// tidy both remove it.
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
	mode := s.mode()
	act := mode == Cancel || mode == Tidy
	s.reconcilePods(ctx, byLease, act, &out)
	s.reconcileEgress(byLease, act, &out)
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

// reconcileEgress stops a `mesh lease-egress` process no live lease and
// no hand-off names, as reconcilePods removes a pod no actor holds
// (ADR-051 decision 3). It finds them two ways: the runtime's listing
// of the host's lease-egress processes, and the egress a leftover
// lease's actor.json names (the actor names it there before it ever
// leaves preparing, so a live lease is never caught). A lease directory
// carrying handoff.json holds the egress its actor.json and hand-off
// name: a lease mid hand-off is not yet abandoned.
func (s *Supervisor) reconcileEgress(live map[string]Live, act bool, out *Reconciled) {
	if s.Egress == nil {
		return
	}
	held := map[int]bool{}
	for _, l := range live {
		if l.EgressPID != 0 {
			held[l.EgressPID] = true
		}
	}
	named := s.leftoverEgress(live, held)
	var found []int
	procs, err := s.Egress.List()
	if err != nil {
		out.Errors = append(out.Errors, "listing egress processes: "+err.Error())
	}
	seen := map[int]bool{}
	for _, p := range procs {
		seen[p.PID] = true
		found = append(found, p.PID)
	}
	for _, n := range named {
		if !seen[n] {
			seen[n] = true
			found = append(found, n)
		}
	}
	for _, pid := range found {
		if held[pid] {
			continue
		}
		out.EgressNoActor = append(out.EgressNoActor, pid)
		if act {
			if err := s.Egress.Stop(pid); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("stopping egress process %d: %v", pid, err))
			}
		}
	}
}

// leftoverEgress reads each lease directory under LeaseRoot that no live
// lease holds: the egress its actor.json names is returned, unless the
// directory carries a hand-off, whose egress (its actor.json's and its
// handoff.json's) is marked held instead.
func (s *Supervisor) leftoverEgress(live map[string]Live, held map[int]bool) []int {
	if s.LeaseRoot == "" {
		return nil
	}
	ents, err := os.ReadDir(s.LeaseRoot)
	if err != nil {
		return nil
	}
	var named []int
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, ok := live[e.Name()]; ok {
			continue
		}
		dir := filepath.Join(s.LeaseRoot, e.Name())
		pid := 0
		if rec, err := readActorRecord(filepath.Join(dir, actor.FileName)); err == nil {
			pid = rec.EgressPID
		}
		if b, err := os.ReadFile(filepath.Join(dir, handoffFileName)); err == nil {
			var h struct {
				EgressPID int `json:"egressPid"`
			}
			_ = json.Unmarshal(b, &h)
			for _, p := range []int{pid, h.EgressPID} {
				if p != 0 {
					held[p] = true
				}
			}
			continue
		}
		if pid != 0 {
			named = append(named, pid)
		}
	}
	return named
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

// readActorRecord reads a lease's actor.json at path.
func readActorRecord(path string) (actor.Record, error) {
	var rec actor.Record
	b, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	err = json.Unmarshal(b, &rec)
	return rec, err
}
