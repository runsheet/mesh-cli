package nodev1

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// The operator's drain (`mesh node drain` on a v1 node; ADR-044
// amendment, drain): the node takes no new lease and tells Mesh so (POST
// /v1/nodes/{id}/drain), so the take stops offering it; its running
// leases finish as they would; once none runs Run ends nil and the
// process exits 0, for the service manager to start the binary now on
// disk. A drain with a wait hands back node_update whatever still runs
// when the wait is up. SIGTERM is unchanged: Run's context ends, and
// every lease is handed back node_restart at once.
//
// Mesh keeps a drain across a restart (only drain false clears it), so
// a drain the node asked for is marked under the work root
// (DrainedMarker) and the next start, finding the mark, asks Mesh to
// resume it before it takes or beats. A drain Mesh holds that this node
// did not ask for is left alone.

// DrainedMarker is the file under the work root that says this node
// asked Mesh to drain it, so its next start resumes.
const DrainedMarker = "mesh-drained"

// errDrainWait is the operator's drain stopping a lease that did not end
// within its wait: handed back node_update, as the self-update's is.
var errDrainWait = fmt.Errorf("the drain's wait is up: %w", errNodeUpdate)

// Status is what the node says of itself for `mesh node status`: its
// registration, whether it drains and the leases it runs.
type Status struct {
	PID         int           `json:"pid"`
	NodeID      string        `json:"node_id,omitempty"`
	Name        string        `json:"name"`
	Version     string        `json:"version"`
	Draining    bool          `json:"draining"`
	DrainReason string        `json:"drain_reason,omitempty"`
	DrainSince  *time.Time    `json:"drain_since,omitempty"`
	HandBackAt  *time.Time    `json:"hand_back_at,omitempty"`
	Leases      []LeaseStatus `json:"leases"`
	At          time.Time     `json:"at"`
}

// LeaseStatus is one lease the node runs.
type LeaseStatus struct {
	LeaseID string    `json:"lease_id"`
	JobID   string    `json:"job_id"`
	Kind    string    `json:"kind"`
	Since   time.Time `json:"since"`
}

// Drain is the operator's drain: the node takes no new lease, asks Mesh
// to stop offering it any, and Run ends nil once its leases have ended.
// A wait above zero hands back node_update what still runs when it is
// up. A second drain may shorten the wait, never lengthen it.
func (r *Runtime) Drain(why string, wait time.Duration) {
	if why == "" {
		why = "the operator drained the node"
	}
	now := time.Now()
	r.mu.Lock()
	first := !r.drain
	r.drain = true
	if r.drainSince.IsZero() {
		r.drainSince = now
	}
	if r.drainReason == "" {
		r.drainReason = why
	}
	if wait > 0 {
		at := now.Add(wait)
		if r.handBackAt.IsZero() || at.Before(r.handBackAt) {
			r.handBackAt = at
		}
	}
	r.askedDrain = true
	nodeID := r.nodeID
	r.signalDrainLocked()
	r.mu.Unlock()
	if wait > 0 {
		r.logf("nodev1: %s; the node takes no more leases, and hands back node_update what still runs after %s", why, wait)
	} else {
		r.logf("nodev1: %s; the node takes no more leases and exits once its leases end", why)
	}
	if first && nodeID != "" {
		// Run waits for Mesh's answer before it ends
		r.telling.Add(1)
		go func() {
			defer r.telling.Done()
			r.tellMeshDrain(nodeID, why)
		}()
	}
	r.publishStatus()
}

// tellMeshDrain asks Mesh to stop offering the node leases, marking the
// work root first so the next start resumes it even if this process
// ends before Mesh answers.
func (r *Runtime) tellMeshDrain(nodeID, why string) {
	if err := os.WriteFile(filepath.Join(r.WorkRoot, DrainedMarker), []byte(nodeID+"\n"), 0o600); err != nil {
		r.logf("nodev1: marking the drain: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	if _, err := r.API.Drain(ctx, nodeID, meshapi.DrainRequest{Drain: true, Reason: why}); err != nil {
		r.logf("nodev1: telling Mesh the node drains: %v (the node takes nothing either way)", err)
		return
	}
	r.logf("nodev1: Mesh holds the node drained: it offers it nothing")
}

// resumeOwnDrain clears, before the first take or beat, a drain this
// node asked Mesh for before it restarted; a drain it never asked for
// stays.
func (r *Runtime) resumeOwnDrain(ctx context.Context, nodeID string) {
	marker := filepath.Join(r.WorkRoot, DrainedMarker)
	if _, err := os.Stat(marker); err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	if _, err := r.API.Drain(cctx, nodeID, meshapi.DrainRequest{Drain: false}); err != nil {
		// the mark stays: the next start asks again
		r.logf("nodev1: asking Mesh to resume the node it drained before its restart: %v", err)
		return
	}
	_ = os.Remove(marker)
	r.logf("nodev1: Mesh resumed the node it drained before its restart")
}

// markDrain is Mesh asking the node to drain (its heartbeat's answer or
// its event stream).
func (r *Runtime) markDrain(why string) {
	r.mu.Lock()
	first := !r.drain
	r.drain = true
	if r.drainSince.IsZero() {
		r.drainSince = time.Now()
	}
	if r.drainReason == "" {
		r.drainReason = why
	}
	r.signalDrainLocked()
	r.mu.Unlock()
	if first {
		r.publishStatus()
	}
}

// signalDrainLocked closes drained, once; r.mu is held.
func (r *Runtime) signalDrainLocked() {
	if r.drained == nil {
		r.drained = make(chan struct{})
	}
	select {
	case <-r.drained:
	default:
		close(r.drained)
	}
}

// drainedCh closes when the node drains.
func (r *Runtime) drainedCh() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drained == nil {
		r.drained = make(chan struct{})
	}
	return r.drained
}

// untilDrain is ctx, ended too when the node drains: a wait for a slot
// stops at a drain rather than offering the node again.
func (r *Runtime) untilDrain(ctx context.Context) (context.Context, context.CancelFunc) {
	cctx, cancel := context.WithCancel(ctx)
	drained := r.drainedCh()
	go func() {
		select {
		case <-drained:
			cancel()
		case <-cctx.Done():
		}
	}()
	return cctx, cancel
}

// handBackWhenWaitIsUp hands back node_update what still runs once a
// drain's wait is up; it watches until ctx ends.
func (r *Runtime) handBackWhenWaitIsUp(ctx context.Context) {
	t := time.NewTicker(r.drainTick())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r.mu.Lock()
		at, live := r.handBackAt, r.leases.Len()
		due := !at.IsZero() && !time.Now().Before(at) && !r.handedBack
		if due {
			r.handedBack = true
		}
		r.mu.Unlock()
		if !due {
			continue
		}
		if live > 0 {
			r.logf("nodev1: the drain's wait is up; handing back node_update the %d lease(s) still running", live)
		}
		r.handBackAll("the node drained for a restart and its wait was up", errDrainWait)
	}
}

func (r *Runtime) drainTick() time.Duration {
	if r.DrainTick > 0 {
		return r.DrainTick
	}
	return time.Second
}

// handBackAll stops every live lease, each handed back node_update with
// why as its reason.
func (r *Runtime) handBackAll(why string, cause error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.leases.All() {
		l := e.Value
		if l.cancelled == "" {
			l.cancelled = why
		}
		if l.actor != nil {
			l.actor.Cancel(why)
		}
		l.cancel(cause)
	}
}

// Status is the node as it stands now.
func (r *Runtime) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Status{
		PID: os.Getpid(), NodeID: r.nodeID, Name: r.Node.Name, Version: r.Node.Version,
		Draining: r.drain, DrainReason: r.drainReason, Leases: []LeaseStatus{}, At: time.Now().UTC(),
	}
	if !r.drainSince.IsZero() {
		t := r.drainSince.UTC()
		s.DrainSince = &t
	}
	if !r.handBackAt.IsZero() {
		t := r.handBackAt.UTC()
		s.HandBackAt = &t
	}
	for _, e := range r.leases.All() {
		s.Leases = append(s.Leases, LeaseStatus{LeaseID: e.ID, JobID: e.Value.jobID, Kind: e.Value.kind, Since: e.Value.started.UTC()})
	}
	sort.Slice(s.Leases, func(i, j int) bool { return s.Leases[i].Since.Before(s.Leases[j].Since) })
	return s
}

// publishStatus hands the node's status to OnStatus, when set.
func (r *Runtime) publishStatus() {
	if r.OnStatus != nil {
		r.OnStatus(r.Status())
	}
}

// drainedEnd is the line Run logs as a drain ends it.
func (r *Runtime) drainedEnd() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.askedDrain {
		return "nodev1: drained: every lease has ended; exiting for the service manager to start the binary on disk"
	}
	return "nodev1: Mesh asked the node to drain; it takes no more leases"
}

// DrainLines are `mesh node status`'s lines for a v1 node's status.
func (s Status) DrainLines(now time.Time) []string {
	var out []string
	head := fmt.Sprintf("This machine: Mesh v1 node running (pid %d)", s.PID)
	if s.NodeID != "" {
		head += fmt.Sprintf(", node %s", s.NodeID)
	}
	if s.Name != "" {
		head += fmt.Sprintf(" (%s)", s.Name)
	}
	if s.Version != "" {
		head += ", " + s.Version
	}
	out = append(out, head+".")
	if s.Draining {
		line := "Draining"
		if s.DrainSince != nil {
			line += fmt.Sprintf(" for %s", now.Sub(*s.DrainSince).Round(time.Second))
		}
		if s.DrainReason != "" {
			line += " (" + s.DrainReason + ")"
		}
		line += ": it takes nothing new and exits once its leases end"
		if s.HandBackAt != nil {
			if left := s.HandBackAt.Sub(now); left > 0 {
				line += fmt.Sprintf("; what still runs in %s is handed back node_update", left.Round(time.Second))
			} else {
				line += "; its wait is up, what still ran is handed back node_update"
			}
		}
		out = append(out, line+".")
	}
	if len(s.Leases) == 0 {
		out = append(out, "Leases: none running.")
		return out
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Leases: %d running:", len(s.Leases))
	out = append(out, b.String())
	for _, l := range s.Leases {
		out = append(out, fmt.Sprintf("  %s  job %s  %s  for %s", l.LeaseID, l.JobID, l.Kind, now.Sub(l.Since).Round(time.Second)))
	}
	return out
}
