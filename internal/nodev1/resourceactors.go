package nodev1

import (
	"context"
	"errors"

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
