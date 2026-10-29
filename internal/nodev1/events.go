package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// Reconnect waits for the node's event stream: from the first, doubling
// to the most while Mesh cannot be reached or refuses the stream.
const (
	eventsRetryFirst = time.Second
	eventsRetryMost  = 30 * time.Second
)

// events keeps the node's event stream (GET /v1/nodes/{id}/events;
// projectmesh-io/api wave 1.5) open until ctx ends, connecting again
// whenever it closes. A cancel stops the lease's session at once, and
// the lease is handed back with cause cancelled; a drain stops the node
// taking more. The heartbeat's answer carries the same cancels and
// drain, so a stream Mesh cannot serve (an older Mesh answers 404)
// costs only speed.
func (r *Runtime) events(ctx context.Context) {
	wait := eventsRetryFirst
	for ctx.Err() == nil {
		start := time.Now()
		err := r.API.Events(ctx, r.NodeID(), r.onEvent)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > eventsRetryMost {
			wait = eventsRetryFirst
		}
		var e *meshapi.Error
		switch {
		case err == nil:
			r.logf("nodev1: the event stream closed; connecting again")
		case errors.As(err, &e) && (e.Status == http.StatusNotFound || e.Status == http.StatusMethodNotAllowed):
			wait = eventsRetryMost // this Mesh has no stream: the heartbeat serves
		default:
			r.logf("nodev1: the event stream: %v", err)
		}
		r.sleep(ctx, wait)
		wait = min(wait*2, eventsRetryMost)
	}
}

// onEvent acts on one event of the stream.
func (r *Runtime) onEvent(e meshapi.NodeEvent) {
	switch e.Event {
	case meshapi.EventCancel:
		var c meshapi.CancelEvent
		if err := json.Unmarshal(e.Data, &c); err != nil || c.LeaseID == "" {
			r.logf("nodev1: a cancel event names no lease: %s", e.Data)
			return
		}
		why := "Mesh cancelled the job (" + c.Cause + ")"
		if c.Reason != "" {
			why += ": " + c.Reason
		}
		if r.cancelLease(c.LeaseID, why) {
			r.logf("nodev1: lease %s: %s", c.LeaseID, why)
		}
	case meshapi.EventDrain:
		r.mu.Lock()
		r.drain = true
		r.mu.Unlock()
		r.logf("nodev1: Mesh asked the node to drain")
	case meshapi.EventTurn:
		var t meshapi.TurnEvent
		if err := json.Unmarshal(e.Data, &t); err != nil || t.LeaseID == "" || t.TurnID == "" {
			r.logf("nodev1: a turn event names no lease or turn: %s", e.Data)
			return
		}
		r.deliverTurn(t.LeaseID, meshapi.Turn{TurnID: t.TurnID, Seq: t.Seq}, "the event stream")
	default:
		// pause is the actor node's (wave 3)
		r.logf("nodev1: event %s ignored by this runtime", e.Event)
	}
}
