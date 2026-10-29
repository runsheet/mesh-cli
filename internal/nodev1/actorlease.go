package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/actor"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// The job actor (ADR-044, Mesh v1 PR 3.2; `mesh node run --actors`):
// each lease runs as an internal/actor.Actor that owns its job
// directory, its room in the scheduler, its pod, its relay grant and
// sessionproxy and its renewal, and writes actor.json before each state
// change. Whatever ends the lease — finish, failure, cancel, timeout or
// panic — the actor reports it and then releases everything it holds;
// a panic is recovered into that release. A session or pod ended by
// SIGTERM or SIGKILL (exit 143 or 137) that the actor did not cause is
// handed back pod_killed, never failed. A node that stops without
// releasing leaves actor.json behind, and the next start hands those
// leases back node_restart and releases what they held
// (releaseLeftovers).

// PodLeaseLabel labels a lease's pod with its lease, so the node can
// tell its own pods (and their actors) from any other.
const PodLeaseLabel = "mesh.v1.lease"

// errPodKilled is the lease's pod ending under the actor: its agent
// exited 137 or 143 and the actor had not stopped it.
var errPodKilled = errors.New("the lease's pod was killed")

// podWatchEvery is how often an actor reads its pod's agent; 5 s when
// Runtime.PodWatchEvery is zero.
func (r *Runtime) podWatchEvery() time.Duration {
	if r.PodWatchEvery > 0 {
		return r.PodWatchEvery
	}
	return 5 * time.Second
}

// leaseDir is a lease's job directory, keyed by the lease.
func (r *Runtime) leaseDir(id string) string { return filepath.Join(r.WorkRoot, "leases", id) }

// runActor runs one lease as a job actor: taken, preparing (its room,
// its pod, then work's session tokens, grant, checkout and proxy),
// running (the session), reporting, releasing, done.
func (r *Runtime) runActor(ctx context.Context, tk *meshapi.Take, room *scheduler.Lease, roomErr error) {
	b, lease := tk.Brief, tk.Lease
	id := lease.LeaseID
	r.logf("nodev1: took lease %s of job %s (%s, %s/%s) as a job actor", id, b.JobID, b.Kind, b.Subject.Type, b.Subject.ID)
	a, err := actor.Start(r.leaseDir(id), actor.Record{LeaseID: id, JobID: b.JobID, Kind: b.Kind})
	if err != nil {
		room.Release()
		r.handBack(id, meshapi.CauseCannotRun, err.Error(), "")
		return
	}
	a.Logf = r.Logf
	// releasing is the only exit: it runs last, whatever came before
	defer a.Release()
	if room != nil {
		a.Hold("room", func() error { room.Release(); return nil })
	}
	leaseCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	mb := NewMailbox()
	r.mu.Lock()
	r.leases.Put(id, &running{cancel: cancel, progressAt: time.Now(), mailbox: mb, turnTool: turnToolFor(b), jobID: b.JobID, actor: a})
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.leases.Delete(id)
		r.mu.Unlock()
	}()
	// a panic anywhere in the lease is recovered into its report and
	// then its release
	defer func() {
		if p := recover(); p != nil {
			r.logf("nodev1: lease %s panicked: %v\n%s", id, p, debug.Stack())
			end := actor.End{Report: actor.ReportHandBack, Cause: meshapi.CausePanic, Reason: fmt.Sprintf("the node panicked: %v", p)}
			r.endActor(a, id, end, func() { r.handBack(id, end.Cause, end.Reason, "") })
		}
	}()
	if roomErr != nil {
		end := actor.End{Report: actor.ReportHandBack, Cause: meshapi.CauseCannotRun, Reason: "the node had no room for the lease: " + roomErr.Error()}
		r.endActor(a, id, end, func() { r.handBack(id, end.Cause, end.Reason, "") })
		return
	}

	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		r.renew(leaseCtx, lease, cancel, room)
	}()
	var out outcome
	if err := a.Enter(actor.Preparing); err != nil {
		out = outcome{err: err, cause: context.Cause(leaseCtx)}
	} else {
		pod, err := r.openPod(leaseCtx, b, a)
		if err != nil {
			out = outcome{err: fmt.Errorf("the lease's pod: %w", err), cause: context.Cause(leaseCtx)}
		} else {
			if pod != "" {
				r.mu.Lock()
				if l := r.lease(id); l != nil {
					l.pod = pod
				}
				r.mu.Unlock()
				go r.watchPod(leaseCtx, pod, cancel)
			}
			out = r.work(leaseCtx, b, mb, a, pod)
		}
	}
	cancel(nil)
	<-renewDone
	out.nodeEnding = ctx.Err() != nil
	// the session's way out ends before Mesh hears the lease is over: the
	// proxy closes and the grant's capability answers 404, its git tokens
	// revoked (the spec's first layer of tokens ending with the lease)
	a.ReleaseNow(holdProxy, holdGrant)
	out.access = r.takeAccess(id)
	if err := a.Enter(actor.Reporting); err != nil {
		r.logf("nodev1: lease %s: %v", id, err)
	}
	end := r.report(id, b, out)
	a.Ended(end)
	a.Reported()
	r.settleThread(b, out.result.SessionID, end.Cause == meshapi.CauseParked)
}

// endActor reports an end the actor reached before its session ran.
func (r *Runtime) endActor(a *actor.Actor, id string, end actor.End, report func()) {
	if a.State() != actor.Reporting {
		if err := a.Enter(actor.Reporting); err != nil {
			r.logf("nodev1: lease %s: %v", id, err)
		}
	}
	a.Ended(end)
	report()
	a.Reported()
}

// podSpec is one of the brief's pods: a service its session needs
// beside it (a database), run as a sidecar in the lease's pod.
type podSpec struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	Env     []string `json:"env,omitempty"`
	Command []string `json:"command,omitempty"`
}

var unsafePodName = regexp.MustCompile(`[^a-z0-9-]+`)

// PodName is a lease's pod.
func PodName(leaseID string) string {
	return "mesh-v1-" + strings.Trim(unsafePodName.ReplaceAllString(strings.ToLower(leaseID), "-"), "-")
}

// openPod makes the lease's pod when the brief names pods and the node
// has a sandbox runtime, with each pod a sidecar in it, and has the
// actor hold it; "" when there is none to make.
func (r *Runtime) openPod(ctx context.Context, b *meshapi.Brief, a *actor.Actor) (string, error) {
	if r.Sandbox == nil || len(b.Pods) == 0 {
		return "", nil
	}
	specs := make([]podSpec, 0, len(b.Pods))
	for i, raw := range b.Pods {
		var p podSpec
		if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" || p.Image == "" {
			return "", fmt.Errorf("the brief's pod %d is not {name, image, env?, command?}: %s", i, raw)
		}
		specs = append(specs, p)
	}
	pod, err := r.Sandbox.CreatePod(ctx, sandbox.PodSpec{Name: PodName(b.LeaseID), Labels: map[string]string{PodLeaseLabel: b.LeaseID}, Image: r.PodImage})
	if err != nil {
		return "", err
	}
	a.HoldPod(pod.Name, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
		defer cancel()
		return r.Sandbox.Remove(ctx, pod)
	})
	for _, p := range specs {
		if err := r.Sandbox.AddSidecar(ctx, pod, sandbox.SidecarSpec{Name: p.Name, Image: p.Image, Env: p.Env, Command: p.Command}); err != nil {
			return "", fmt.Errorf("the pod's %s: %w", p.Name, err)
		}
	}
	return pod.Name, nil
}

// watchPod reads the lease's pod's agent until ctx ends: an agent that
// exited 137 or 143 while the actor still ran it was killed from
// outside, and the lease ends pod_killed.
func (r *Runtime) watchPod(ctx context.Context, pod string, cancel context.CancelCauseFunc) {
	t := time.NewTicker(r.podWatchEvery())
	defer t.Stop()
	agent := sandbox.AgentName(sandbox.Pod{Name: pod})
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := r.Sandbox.AgentState(ctx, agent)
		if err != nil || st.Running || !actor.KilledExit(st.Exit.Code) {
			continue
		}
		if ctx.Err() == nil {
			cancel(fmt.Errorf("%w: its agent %s exited %d", errPodKilled, agent, st.Exit.Code))
		}
		return
	}
}

// podKilled is why the lease ended pod_killed, when it did: its pod's
// agent killed under it, or its session ended by SIGTERM or SIGKILL
// with nothing of the actor's stopping it.
func podKilled(o outcome) (string, bool) {
	if errors.Is(o.cause, errPodKilled) {
		return o.cause.Error(), true
	}
	if o.cause != nil || o.nodeEnding {
		return "", false
	}
	if code, ok := actor.Killed(o.err); ok {
		return fmt.Sprintf("the session was killed (exit %d), not by the node: %v", code, o.err), true
	}
	return "", false
}

// releaseLeftovers finishes what a node that stopped left behind: each
// actor.json not done is a lease the node no longer runs. One not yet
// reported is handed back node_restart (or with the cause it had
// decided), its pods are removed and its job directory with them.
func (r *Runtime) releaseLeftovers() {
	left, err := actor.Leftovers(filepath.Join(r.WorkRoot, "leases"))
	if err != nil {
		r.logf("nodev1: reading leftover actors: %v", err)
		return
	}
	for _, l := range left {
		rec := l.Record
		r.logf("nodev1: lease %s was %s when the node stopped; releasing it", rec.LeaseID, rec.State)
		if !rec.Reported {
			cause, reason := meshapi.CauseNodeRestart, fmt.Sprintf("the node restarted while the lease was %s", rec.State)
			if e := rec.End; e != nil && e.Report == actor.ReportHandBack && e.Cause != "" {
				cause, reason = e.Cause, e.Reason
			}
			r.handBack(rec.LeaseID, cause, reason, "")
		}
		for _, p := range rec.Pods {
			if r.Sandbox == nil {
				r.logf("nodev1: lease %s left pod %s and the node has no sandbox runtime to remove it", rec.LeaseID, p)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
			if err := r.Sandbox.Remove(ctx, sandbox.Pod{Name: p}); err != nil {
				r.logf("nodev1: lease %s: removing pod %s: %v", rec.LeaseID, p, err)
			}
			cancel()
		}
		if err := os.RemoveAll(l.Dir); err != nil {
			r.logf("nodev1: lease %s: removing %s: %v", rec.LeaseID, l.Dir, err)
		}
	}
}
