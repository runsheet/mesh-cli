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
//     holds are removed; adopted sessions are counted. Nothing of a
//     lease younger than YoungAge is touched — not its pods, not its
//     egress process — however the live leases read: a lease being
//     prepared may not yet be named everywhere the reconcile looks.
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
	// DefaultYoungAge is how young a lease — its job directory or
	// actor.json last written, or its egress process started — is
	// spared by the reconcile whatever the live leases say: a lease
	// mid-preparation, the node starting dozens at once, can be seconds
	// from naming its egress process or pod where the reconcile reads
	// them (ADR-051 amendment, 2026-11-16).
	DefaultYoungAge = 5 * time.Minute
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
	// StartedAt is when the process started, zero when the runtime
	// cannot tell: a process younger than YoungAge is never stopped.
	StartedAt time.Time
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
	// YoungAge is how young a lease's job directory, actor.json or
	// egress process is spared by the reconcile (DefaultYoungAge when
	// zero): its pods are not removed and its egress process is not
	// stopped, held by a live lease or not.
	YoungAge time.Duration
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
	// PodsYoung and EgressYoung are the pods and egress processes the
	// reconcile would have taken but spared, being younger than
	// YoungAge.
	PodsYoung   []string
	EgressYoung []int
	Errors      []string
}

// Line is the reconcile's log line.
func (r Reconciled) Line() string {
	return fmt.Sprintf("supervisor: reconcile: %d actors (%d adopted); removed %d pods with no actor, %d with only a sidecar, %d job directories and %d grants no actor held; stopped %d egress process(es) with no actor and no hand-off",
		r.Actors, r.Adopted, len(r.PodsNoActor), len(r.PodsSidecarOnly), len(r.Workspaces), len(r.Grants), len(r.EgressNoActor)) + r.youngLine()
}

func (r Reconciled) youngLine() string {
	if len(r.PodsYoung) == 0 && len(r.EgressYoung) == 0 {
		return ""
	}
	return fmt.Sprintf("; spared %d pod(s) and %d egress process(es) of leases too young to judge", len(r.PodsYoung), len(r.EgressYoung))
}

// youngAge is YoungAge, or DefaultYoungAge.
func (s *Supervisor) youngAge() time.Duration { return or(s.YoungAge, DefaultYoungAge) }

// young says whether t is within YoungAge of now; a time ahead of now
// (a clock stepped back) is young too.
func (s *Supervisor) young(t time.Time) bool {
	return !t.IsZero() && s.now().Sub(t) < s.youngAge()
}

// youngLease says whether the lease's job directory under LeaseRoot, or
// its actor.json (rewritten at every state change), was written within
// YoungAge: a lease the node took moments ago, which may not yet name
// its pod or egress process where the reconcile reads them.
func (s *Supervisor) youngLease(lease string) bool {
	if s.LeaseRoot == "" || lease == "" || strings.ContainsAny(lease, `/\`) || lease == "." || lease == ".." {
		return false
	}
	dir := filepath.Join(s.LeaseRoot, lease)
	for _, p := range []string{dir, filepath.Join(dir, actor.FileName)} {
		if info, err := os.Stat(p); err == nil && s.young(info.ModTime()) {
			return true
		}
	}
	return false
}

// Reconcile is one reconcile of the node against its actors. In mode
// warn it removes nothing and logs what it would; modes cancel and
// tidy both remove it.
//
// Each part reads the live leases afresh after it has listed what it
// judges — pods, egress processes, job directories, grants — never from
// a snapshot taken before: a lease the node takes while the reconcile
// lists (a podman listing on a loaded node takes seconds) is live by
// the time its pod or egress process is judged, and is never taken for
// one no actor holds. On 2026-11-16 a snapshot taken before the
// listings stopped new leases' egress processes seconds after they
// started, and the leases were handed back egress_died.
func (s *Supervisor) Reconcile(ctx context.Context) Reconciled {
	var out Reconciled
	for _, l := range s.Fleet.Live() {
		out.Actors++
		if l.Adopted {
			out.Adopted++
		}
	}
	mode := s.mode()
	act := mode == Cancel || mode == Tidy
	s.reconcilePods(ctx, act, &out)
	s.reconcileEgress(act, &out)
	s.reconcileWorkspaces(act, &out)
	grants := s.Fleet.Grants()
	byLease := s.liveByLease()
	for _, id := range grants {
		if _, ok := byLease[id]; ok {
			continue
		}
		// a grant closed under a checkout answers its git "no such
		// relay": a young lease's is never closed
		if s.youngLease(id) {
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

// liveByLease is the live leases as they stand now, by lease.
func (s *Supervisor) liveByLease() map[string]Live {
	out := map[string]Live{}
	for _, l := range s.Fleet.Live() {
		out[l.LeaseID] = l
	}
	return out
}

// reconcilePods removes the node's pods no actor holds, and those whose
// agent has ended leaving a sidecar running under an actor still
// running its session.
func (s *Supervisor) reconcilePods(ctx context.Context, act bool, out *Reconciled) {
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
	// the live leases once the listing is in hand (Reconcile)
	live := s.liveByLease()
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
			if s.youngLease(lease) {
				out.PodsYoung = append(out.PodsYoung, p.Name)
				continue
			}
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
func (s *Supervisor) reconcileEgress(act bool, out *Reconciled) {
	if s.Egress == nil {
		return
	}
	procs, err := s.Egress.List()
	if err != nil {
		out.Errors = append(out.Errors, "listing egress processes: "+err.Error())
	}
	// the live leases once the listing is in hand (Reconcile)
	live := s.liveByLease()
	held := map[int]bool{}
	for _, l := range live {
		if l.EgressPID != 0 {
			held[l.EgressPID] = true
		}
	}
	spare := map[int]bool{}
	named := s.leftoverEgress(live, held, spare)
	var found []int
	seen := map[int]bool{}
	for _, p := range procs {
		seen[p.PID] = true
		found = append(found, p.PID)
		if s.young(p.StartedAt) {
			spare[p.PID] = true
		}
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
		if spare[pid] {
			out.EgressYoung = append(out.EgressYoung, pid)
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
// handoff.json's) is marked held instead, or the lease is younger than
// YoungAge, whose egress is returned but marked spare.
func (s *Supervisor) leftoverEgress(live map[string]Live, held, spare map[int]bool) []int {
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
		if pid != 0 && s.youngLease(e.Name()) {
			spare[pid] = true
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
func (s *Supervisor) reconcileWorkspaces(act bool, out *Reconciled) {
	if s.LeaseRoot == "" {
		return
	}
	ents, err := os.ReadDir(s.LeaseRoot)
	if err != nil {
		return
	}
	live := s.liveByLease()
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
