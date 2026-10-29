package nodev1

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A thread's turns reach the live session over the node's event stream,
// and, for a turn the stream missed (it was down when the turn came), in
// the answer of the lease's next renewal (projectmesh-io/api ADR 001
// amendment, threads: "Acknowledged turns"). Each renewal says how far
// the session has the thread (turns_seen), which Mesh takes as the
// node's ack of every turn at or below it. Whichever way a turn comes,
// the session is given it once: the lease's turnLedger keeps the seqs it
// was given.

// turnLedger is one thread lease's record of the turns its session was
// given. seen is the newest seq at or below which the session has every
// turn: the brief's last_turn at the take (the session reads the thread
// so far with the brief tool), then raised by each renewal's answer
// once every turn of it is in the session. given holds the seqs above
// seen the session was given.
type turnLedger struct {
	mu    sync.Mutex
	seen  int64
	given map[int64]bool
	// unsaid is a ledger whose Mesh refused turns_seen: its renewals say
	// nothing of turns.
	unsaid bool
}

// newTurnLedger is the ledger of a brief's lease: nil for a job that is
// not a thread, whose renewals say nothing of turns.
func newTurnLedger(b *meshapi.Brief) *turnLedger {
	if b == nil || b.Thread == nil {
		return nil
	}
	l := &turnLedger{given: map[int64]bool{}}
	if b.LastTurn != nil {
		l.seen = b.LastTurn.Seq
	}
	return l
}

// TurnsSeen is the renewal's turns_seen; nil for a ledger-less lease.
func (l *turnLedger) TurnsSeen() *int64 {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.unsaid {
		return nil
	}
	seen := l.seen
	return &seen
}

// off stops the ledger's renewals saying turns_seen.
func (l *turnLedger) off() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.unsaid = true
}

// give hands the turn to the session through post, unless the session
// already has it; it answers whether post was called. A post that
// fails leaves the turn for the next renewal's answer.
func (l *turnLedger) give(seq int64, post func() error) (bool, error) {
	if l == nil {
		return true, post()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq <= l.seen || l.given[seq] {
		return false, nil
	}
	if err := post(); err != nil {
		return true, err
	}
	l.given[seq] = true
	return true, nil
}

// through raises seen to through once the session has every turn the
// renewal answered; the seqs at or below it are forgotten.
func (l *turnLedger) through(turns []meshapi.Turn, through int64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range turns {
		if t.Seq > l.seen && !l.given[t.Seq] {
			return
		}
	}
	if through <= l.seen {
		return
	}
	l.seen = through
	for seq := range l.given {
		if seq <= through {
			delete(l.given, seq)
		}
	}
}

// deliverTurn gives the lease's session the thread's turn, once: from
// the event stream or a renewal's answer, whichever comes first.
func (r *Runtime) deliverTurn(leaseID string, t meshapi.Turn, via string) {
	tool := r.turnTool(leaseID)
	posted, err := r.leaseTurns(leaseID).give(t.Seq, func() error {
		return r.deliver(leaseID, Message{Kind: MsgTurn, Text: turnLine(tool, t.TurnID, t.Seq)})
	})
	switch {
	case err != nil:
		r.logf("nodev1: turn %s for lease %s: %v", t.TurnID, leaseID, err)
	case posted:
		r.logf("nodev1: lease %s: turn %s (seq %d) went to the session, from %s", leaseID, t.TurnID, t.Seq, via)
	}
}

// renewLease renews the lease, naming how far a thread's session has
// its thread, and gives the session the turns the answer carries. A
// Mesh from before turns_seen refuses a body it does not know (400): the
// lease then renews as before, and its turns come over the event stream
// alone.
func (r *Runtime) renewLease(ctx context.Context, leaseID string) (*meshapi.RenewResponse, error) {
	seen := r.renewTurnsSeen(leaseID)
	resp, err := r.API.Renew(ctx, leaseID, seen)
	var e *meshapi.Error
	if seen != nil && errors.As(err, &e) && e.Status == http.StatusBadRequest {
		r.logf("nodev1: lease %s: Mesh refused the renewal's turns_seen (%v); renewing without it", leaseID, err)
		if l := r.leaseTurns(leaseID); l != nil {
			l.off()
		}
		resp, err = r.API.Renew(ctx, leaseID, nil)
	}
	if err == nil {
		r.renewedTurns(leaseID, resp)
	}
	return resp, err
}

// leaseTurns is the turn ledger of the lease's session, nil for a lease
// the node does not hold or one that is not a thread's.
func (r *Runtime) leaseTurns(leaseID string) *turnLedger {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.lease(leaseID); l != nil && l.mailbox != nil {
		return l.mailbox.turns
	}
	return nil
}

// renewTurnsSeen is the lease's renewal's turns_seen: nil for a lease
// that is not a thread's.
func (r *Runtime) renewTurnsSeen(leaseID string) *int64 { return r.leaseTurns(leaseID).TurnsSeen() }

// renewedTurns gives the session the turns a renewal answered, then
// moves the lease's turns_seen on to the seq Mesh looked through.
func (r *Runtime) renewedTurns(leaseID string, resp *meshapi.RenewResponse) {
	l := r.leaseTurns(leaseID)
	if l == nil || resp == nil {
		return
	}
	for _, t := range resp.Turns {
		r.deliverTurn(leaseID, t, "the renewal")
	}
	l.through(resp.Turns, resp.TurnsThrough)
}
