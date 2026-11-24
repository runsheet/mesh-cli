package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// The assistant dispatcher (projectmesh-io/api ADR 001 amendment,
// assistant dispatcher; goal one-assistant-dispatcher-per-org-answers-
// every-ask P2 to P4). A dispatcher kind's lease is long: it is one warm
// job per tenant, and the node, not a model, is the dispatcher. Each of
// its turns of kind ask is one ask, and the node answers each through a
// sub-agent of its own — a one-shot session in the lease's pod, on the
// ask's own Latchkey session (POST /v1/leases/{id}/asks/{ask}/session),
// never the dispatcher's — then finishes it (POST .../asks/{ask}/finish),
// which acks its turn.
//
// At most the dispatcher's cap of sub-agents run at once; the asks past
// it wait in the lease's FIFO queue, in memory. An ask is acked only by
// its finish: the lease's turns_seen never passes an ask still queued or
// running, so a node that stops — a restart, an upgrade, a kill — leaves
// every ask it had not finished unacked, and the next lease's renewals
// bring them back (its brief's dispatcher.acked_through is where it
// starts). A lease with no ask for its thread's park time ends parked;
// the next ask wakes it.

// DefaultDispatcherCap is how many sub-agents a dispatcher runs at once
// when neither its kind nor the node names a cap.
const DefaultDispatcherCap = 4

// finishAskTries bounds the finishes of one ask: one Mesh did not take
// is left unacked, and comes back to the next lease.
const finishAskTries = 3

// finishAskTimeout bounds each of them, past the lease's own end.
const finishAskTimeout = 30 * time.Second

// askEnd is how a sub-agent left its ask.
type askEnd struct {
	end    string
	result json.RawMessage
}

// errAskNotRun is a sub-agent that never had its ask: the lease ended
// first. The ask is not finished, and stays unacked.
var errAskNotRun = errors.New("the dispatcher's lease ended before the ask was answered")

// askQueue is one dispatcher lease's asks: every ask it was given and
// has not finished (outstanding), the ones waiting for a sub-agent in the
// order they came (queued), the ones a sub-agent runs (running), and
// through, the seq Mesh last looked through for the lease. It is the
// lease's turn ledger: seen, its turns_seen, never passes an outstanding
// ask.
type askQueue struct {
	mu          sync.Mutex
	cap         int
	through     int64
	queued      []meshapi.Turn
	running     map[int64]bool
	outstanding map[int64]bool
	finished    map[string]bool
	// quietSince is when the queue last had no outstanding ask; zero
	// until Mesh first looked through the lease's turns (raise), so a
	// lease never parks before it has heard of the asks waiting for it
	quietSince time.Time
	looked     bool
	// unsaid stops the renewals saying turns_seen, for a Mesh that
	// refused it
	unsaid bool
	wake   chan struct{}
	now    func() time.Time
}

// newAskQueue is the queue of a dispatcher's lease: cap sub-agents at
// once, and through the brief's acked_through.
func newAskQueue(cap int, through int64, now func() time.Time) *askQueue {
	if cap < 1 {
		cap = DefaultDispatcherCap
	}
	if now == nil {
		now = time.Now
	}
	return &askQueue{cap: cap, through: through, running: map[int64]bool{}, outstanding: map[int64]bool{},
		finished: map[string]bool{}, wake: make(chan struct{}, 1), now: now}
}

// dispatcherCap is the cap a dispatcher's lease runs at: its kind's,
// never more than the node's setting (DefaultDispatcherCap when unset).
func dispatcherCap(kind, node int) int {
	if node < 1 {
		node = DefaultDispatcherCap
	}
	if kind < 1 || kind > node {
		return node
	}
	return kind
}

func (q *askQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// offer queues an ask turn, once: whichever of the event stream or a
// renewal brings it first. A turn of another kind, an ask the lease has
// or finished, and an ask at or below seen are not queued; offer
// answers whether t was.
func (q *askQueue) offer(t meshapi.Turn) bool {
	if t.Kind != meshapi.TurnKindAsk || t.TurnID == "" || t.Seq < 1 {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.outstanding[t.Seq] || q.finished[t.TurnID] || t.Seq <= q.seenLocked() {
		return false
	}
	q.outstanding[t.Seq] = true
	q.queued = append(q.queued, t)
	q.signal()
	return true
}

// next is the oldest queued ask, when a sub-agent may start for it.
func (q *askQueue) next() (meshapi.Turn, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.queued) == 0 || len(q.running) >= q.cap {
		return meshapi.Turn{}, false
	}
	t := q.queued[0]
	q.queued = q.queued[1:]
	q.running[t.Seq] = true
	return t, true
}

// done is a sub-agent's end: finished, its ask is no longer outstanding
// and seen may pass it; not finished, it stays outstanding — never run
// again by this lease, and below the lease's turns_seen, so the next
// lease has it.
func (q *askQueue) done(t meshapi.Turn, finished bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.running, t.Seq)
	if finished {
		delete(q.outstanding, t.Seq)
		q.finished[t.TurnID] = true
		if len(q.outstanding) == 0 {
			q.quietSince = q.now()
		}
	}
	q.signal()
}

// raise moves through on to the seq a renewal looked through.
func (q *askQueue) raise(through int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if through > q.through {
		q.through = through
	}
	if !q.looked {
		q.looked = true
		if len(q.outstanding) == 0 {
			q.quietSince = q.now()
		}
		q.signal()
	}
}

// seenLocked is the lease's turns_seen: through, held below the oldest
// outstanding ask.
func (q *askQueue) seenLocked() int64 {
	seen := q.through
	for seq := range q.outstanding {
		if seq-1 < seen {
			seen = seq - 1
		}
	}
	return seen
}

// TurnsSeen is the renewal's turns_seen.
func (q *askQueue) TurnsSeen() *int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.unsaid {
		return nil
	}
	seen := q.seenLocked()
	return &seen
}

// off stops the renewals saying turns_seen.
func (q *askQueue) off() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.unsaid = true
}

// counts are the asks running, queued, and outstanding.
func (q *askQueue) counts() (running, queued, outstanding int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.running), len(q.queued), len(q.outstanding)
}

// quietFor is how long the queue has had no outstanding ask; 0 while it
// has one.
func (q *askQueue) quietFor() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.outstanding) > 0 || !q.looked {
		return 0
	}
	return max(q.now().Sub(q.quietSince), time.Nanosecond)
}

// dispatcher runs a lease's asks: run answers one through a sub-agent,
// finish tells Mesh how it ended.
type dispatcher struct {
	lease  string
	q      *askQueue
	run    func(ctx context.Context, t meshapi.Turn) (askEnd, error)
	finish func(ctx context.Context, t meshapi.Turn, e askEnd) error
	// parkAfter is how long the lease may have no ask before it ends
	// parked; 0 never parks
	parkAfter time.Duration
	// started, set, hears each ask as a sub-agent is started for it, in
	// the order they start
	started func(meshapi.Turn)
	logf    func(string, ...any)
}

// serve runs the lease's asks until ctx ends (the lease's end: ctx's
// error) or the lease has had no ask for parkAfter (parked, nil). It
// returns once every sub-agent it started has ended.
func (d *dispatcher) serve(ctx context.Context) (parked bool, err error) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		for {
			t, ok := d.q.next()
			if !ok {
				break
			}
			if d.started != nil {
				d.started(t)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				d.one(ctx, t)
			}()
		}
		var timer *time.Timer
		var tick <-chan time.Time
		if d.parkAfter > 0 {
			quiet := d.q.quietFor()
			if quiet > 0 && quiet >= d.parkAfter {
				return true, nil
			}
			wait := d.parkAfter - quiet
			if quiet == 0 {
				wait = d.parkAfter
			}
			timer = time.NewTimer(wait)
			tick = timer.C
		}
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return false, ctx.Err()
		case <-d.q.wake:
		case <-tick:
		}
		stopTimer(timer)
	}
}

// one answers t through a sub-agent and finishes it. An ask the lease
// ended under, and one whose finish Mesh never took, stay unacked.
func (d *dispatcher) one(ctx context.Context, t meshapi.Turn) {
	running, queued, _ := d.q.counts()
	d.logf("nodev1: dispatcher %s: ask %s (seq %d) starts a sub-agent (%d running, %d queued)", d.lease, t.TurnID, t.Seq, running, queued)
	e, err := d.run(ctx, t)
	if err != nil {
		d.logf("nodev1: dispatcher %s: ask %s (seq %d) not answered: %v; it stays unacked for the next lease", d.lease, t.TurnID, t.Seq, err)
		d.q.done(t, false)
		return
	}
	var ferr error
	for try := 0; try < finishAskTries; try++ {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishAskTimeout)
		ferr = d.finish(fctx, t, e)
		cancel()
		if ferr == nil {
			break
		}
		var me *meshapi.Error
		if errors.As(ferr, &me) && me.Status >= 400 && me.Status < 500 {
			break // Mesh refused it: a lease no longer the job's, an ask not its own
		}
	}
	if ferr != nil {
		d.logf("nodev1: dispatcher %s: ask %s (seq %d) ended %s but Mesh did not take its finish: %v; it stays unacked for the next lease", d.lease, t.TurnID, t.Seq, e.end, ferr)
		d.q.done(t, false)
		return
	}
	d.logf("nodev1: dispatcher %s: ask %s (seq %d) finished %s", d.lease, t.TurnID, t.Seq, e.end)
	d.q.done(t, true)
}

func stopTimer(t *time.Timer) {
	if t != nil {
		t.Stop()
	}
}
