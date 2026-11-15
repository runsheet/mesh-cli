package nodev1

import (
	"context"
	"time"

	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/nodetools"
)

// Step progress (ADR-044 amendment, 2026-11-16; ADR-049 amendment):
// Mesh hands back a lease whose progress_at is its stall limit old
// (projectmesh-io/api #192, MESH_STALL_LIMIT), and a heartbeat is not
// progress. A session's stream events move progress_at (touch), but a
// lease is also working when no event streams: while a run_suite unit
// writes output or test events, while a lane opens or waits for its
// room, and while a node tool's call is in flight. Each of those moves
// the lease's progress_at too, at most once a stepProgressEvery per
// lease.
//
// What does not: a run_suite run that writes nothing. Its call may be
// in flight and its command alive, but a command that is alive and
// silent is exactly what a hung one looks like, so run_suite speaks
// through its run's output alone and a silent run shows Mesh no
// progress — the stall limit is for it.

// stepProgressEvery is the least time between two movements of a
// lease's progress by its command steps.
const stepProgressEvery = time.Minute

// clock is the runtime's time: now when a test set one.
func (r *Runtime) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// stepProgress is the lease's command step showing it works: what names
// the step. It moves the lease's progress_at for the next heartbeat, at
// most once a stepProgressEvery; a lease the node no longer holds has
// none to move. It answers whether it moved.
func (r *Runtime) stepProgress(id, what string) bool {
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.lease(id)
	if l == nil {
		return false
	}
	if !l.stepAt.IsZero() && now.Sub(l.stepAt) < stepProgressEvery {
		return false
	}
	l.stepAt, l.step = now, what
	if now.After(l.progressAt) {
		l.progressAt = now
	}
	return true
}

// stillRunning says the lease is still running what, now and every
// stepProgressEvery until the func it answers is called, once, or ctx
// ends.
func (r *Runtime) stillRunning(ctx context.Context, id, what string) (done func()) {
	r.stepProgress(id, what)
	every := r.stepBeat
	if every <= 0 {
		every = stepProgressEvery
	}
	end, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-end:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				r.stepProgress(id, what)
			}
		}
	}()
	// nothing moves once done has answered
	return func() { close(end); <-ended }
}

// reportsWhileRunning is tools, each call of which moves the lease's
// progress while it is in flight: "still running <tool>", until the
// call answers, its own ctx ends (the session gave up on it) or the
// lease's does. run_suite is left as it is: its call waits on a run
// that may be hung, and the run's own output is its progress
// (RunSuite.OnActivity).
func (r *Runtime) reportsWhileRunning(lease context.Context, id string, tools []mcptool.Tool) []mcptool.Tool {
	if lease == nil {
		lease = context.Background()
	}
	for i := range tools {
		if tools[i].Name == nodetools.RunSuiteTool || tools[i].Call == nil {
			continue
		}
		name, call := tools[i].Name, tools[i].Call
		tools[i].Call = func(ctx context.Context, args map[string]any) (string, error) {
			wctx, cancel := context.WithCancel(ctx)
			defer cancel()
			stop := context.AfterFunc(lease, cancel)
			defer stop()
			done := r.stillRunning(wctx, id, "still running "+name)
			defer done()
			return call(ctx, args)
		}
	}
	return tools
}
