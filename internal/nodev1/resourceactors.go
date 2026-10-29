package nodev1

import (
	"context"
	"errors"
	"time"

	"projectmesh.io/mesh-cli/internal/resources"
)

// The resource actors (ADR-044, Mesh v1 PR 3.4): the cache trim runs off
// the take loop holding the build cache exclusively in the scheduler
// (`--cache-trim`), and the self-update drains the node, hands back
// node_update what still runs past its drain wait, installs the release
// and ends Run with ErrRestartForUpdate (`--self-update`).

// ErrRestartForUpdate is Run's end once a self-update installed a new
// binary: the service manager starts it in place.
var ErrRestartForUpdate = errors.New("nodev1: restart for update")

// ErrRolledBack is Run's end when this start rolled a self-update back:
// the previous binary is in place again, and the service manager starts
// it.
var ErrRolledBack = errors.New("nodev1: rolled back a self-update")

// errNodeUpdate is the self-update stopping a lease that did not end on
// its own within the drain wait.
var errNodeUpdate = errors.New("the node is updating")

// updateNode is the runtime as the self-update moves it.
type updateNode struct{ r *Runtime }

var _ resources.Node = updateNode{}

// Drain implements resources.Node: the node takes no more leases and
// waits for the update.
func (n updateNode) Drain(why string) {
	n.r.mu.Lock()
	n.r.drain, n.r.updating = true, true
	n.r.mu.Unlock()
	n.r.logf("nodev1: %s; the node takes no more leases", why)
}

// Live implements resources.Node.
func (n updateNode) Live() int {
	n.r.mu.Lock()
	defer n.r.mu.Unlock()
	return n.r.leases.Len()
}

// HandBackAll implements resources.Node: every live lease is stopped and
// handed back node_update.
func (n updateNode) HandBackAll(why string) {
	n.r.mu.Lock()
	defer n.r.mu.Unlock()
	for _, e := range n.r.leases.All() {
		l := e.Value
		if l.cancelled == "" {
			l.cancelled = why
		}
		if l.actor != nil {
			l.actor.Cancel(why)
		}
		l.cancel(errNodeUpdate)
	}
}

// startResources starts the node's resource actors under ctx; updated
// closes once the self-update ends, holding the version it installed.
func (r *Runtime) startResources(ctx context.Context) {
	if r.CacheTrim != nil && r.Scheduler != nil {
		t := *r.CacheTrim
		t.Scheduler = r.Scheduler
		if t.Logf == nil {
			t.Logf = r.Logf
		}
		go t.Run(ctx)
	}
	r.updated = make(chan string, 1)
	if r.Updater == nil {
		close(r.updated)
		return
	}
	su := &resources.SelfUpdate{Updater: r.Updater, Node: updateNode{r}, Every: r.UpdateEvery, DrainWait: r.DrainWait, Logf: r.Logf}
	go func() {
		defer close(r.updated)
		if v := su.Run(ctx); v != "" {
			r.updated <- v
		}
	}()
}

// awaitUpdate is the take loop meeting a drain: one the self-update
// asked for waits for its install, and Run ends ErrRestartForUpdate when
// it installed; any other drain (Mesh's) ends Run.
func (r *Runtime) awaitUpdate(ctx context.Context) error {
	r.mu.Lock()
	updating := r.updating
	r.mu.Unlock()
	if !updating {
		return nil
	}
	select {
	case v, ok := <-r.updated:
		if ok && v != "" {
			r.logf("nodev1: installed %s; restarting into it", v)
			return ErrRestartForUpdate
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// checkUpdate is the start's look at the update state (`--self-update`,
// Confirm): a pending update counts this start, the third unconfirmed
// one rolls back and ends Run with ErrRolledBack, and the line the
// legacy node logs — "confirming update vX → vY: start 1 of 3, rolls
// back on the third" — is logged first, so a slow confirm is never
// mistaken for a crash.
func (r *Runtime) checkUpdate() error {
	if r.Confirm == nil {
		return nil
	}
	check, err := r.Confirm.Start()
	if err != nil {
		// an unreadable state file is no reason not to run
		r.logf("nodev1: self-update: reading the update state: %v", err)
		return nil
	}
	r.logf("nodev1: %s", check.Line())
	if check.RolledBack {
		return ErrRolledBack
	}
	r.mu.Lock()
	r.confirming = check.Pending
	r.mu.Unlock()
	return nil
}

// isConfirming is whether this start still has an update to confirm.
func (r *Runtime) isConfirming() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.confirming
}

// startFailed records why a start of an unconfirmed update ended before
// it could confirm (its registration failed), for the rollback to name.
func (r *Runtime) startFailed(err error) {
	if !r.isConfirming() {
		return
	}
	if nerr := r.Confirm.StartFailed(err); nerr != nil {
		r.logf("nodev1: self-update: recording the start failure: %v", nerr)
	}
}

// confirmFailed records a heartbeat of an unconfirmed update that did
// not go through.
func (r *Runtime) confirmFailed(err error) {
	if !r.isConfirming() {
		return
	}
	r.logf("nodev1: self-update: not confirmed yet: %v", err)
	if nerr := r.Confirm.Failed(err); nerr != nil {
		r.logf("nodev1: self-update: recording the confirm error: %v", nerr)
	}
}

// confirmUpdate confirms a pending update once Mesh answered a
// heartbeat: the new binary registered and reaches Mesh.
func (r *Runtime) confirmUpdate() {
	r.mu.Lock()
	pending := r.confirming
	r.confirming = false
	r.mu.Unlock()
	if !pending {
		return
	}
	if err := r.Confirm.Confirmed(); err != nil {
		r.logf("nodev1: self-update: recording the confirm: %v", err)
		return
	}
	r.logf("nodev1: self-update: confirmed %s", r.Node.Version)
}

// awaitConfirm, while an update is unconfirmed, sends the first
// heartbeat at once rather than a period later, and ends Run with a
// *resources.UnconfirmedError once the confirm window passes without
// one: the process exits, and the service manager's restart is the
// update's next start.
func (r *Runtime) awaitConfirm(ctx context.Context, stopRun context.CancelCauseFunc) {
	if !r.isConfirming() {
		return
	}
	r.heartbeat(ctx)
	if !r.isConfirming() {
		return
	}
	window := r.Confirm.Window()
	go func() {
		t := time.NewTimer(window)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if r.isConfirming() {
			err := r.Confirm.Unconfirmed()
			r.logf("nodev1: self-update: %v; exiting for the next start", err)
			stopRun(err)
		}
	}()
}
