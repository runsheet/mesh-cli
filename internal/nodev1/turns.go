package nodev1

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A job's turns reach the live session over the node's event stream,
// and, for a turn the stream missed (it was down when the turn came), in
// the answer of the lease's next renewal (projectmesh-io/api ADR 001
// amendment, threads: "Acknowledged turns"). Each renewal says how far
// the session has the job's turns (turns_seen), which Mesh takes as the
// node's ack of every turn at or below it. Whichever way a turn comes,
// the session is given it once: the lease's turnLedger keeps the seqs it
// was given.
//
// Any job whose brief names a turn tool takes turns, not only a thread
// (the same amendment, "Turns into any live job"), and each turn says
// its kind and who it is from: turnMessage is what the session reads,
// and an interrupt stops the session's turn in flight before it — the
// session loop interrupts and sends the turn as one step
// (Message.Interrupt on a MsgTurn), so the interrupted turn's result
// cannot end a one-shot session before the turn is in. A turn
// counts as given only once the session loop put it into the session
// (Message.Sent). A one-shot session's input closes when the product
// answers its finish tool, or when its last turn ends: a turn after that
// is refused by the mailbox (errInputClosed), logged once, and never
// acked — the lease's turns_seen stays below it, so Mesh's acked_at for
// it stays empty and the product can tell the session never had it.

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
	// pending holds the seqs posted to the session loop and not yet
	// sent; closed the seqs refused because the input had closed, logged
	// once.
	pending map[int64]bool
	closed  map[int64]bool
	// unsaid is a ledger whose Mesh refused turns_seen: its renewals say
	// nothing of turns.
	unsaid bool
}

// newTurnLedger is the ledger of a brief's lease: nil for a job that
// takes no turns (no thread, no turn tool), whose renewals say nothing
// of turns.
func newTurnLedger(b *meshapi.Brief) *turnLedger {
	if b == nil || (b.Thread == nil && b.TurnTool == "") {
		return nil
	}
	l := &turnLedger{given: map[int64]bool{}, pending: map[int64]bool{}, closed: map[int64]bool{}}
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
// has it or it is on its way; it answers whether post was called. post
// calls sent once the session loop put the turn into the session (nil)
// or could not (an error). A post that fails, or a turn that was not
// sent, is left for the next renewal's answer.
func (l *turnLedger) give(seq int64, post func(sent func(error)) error) (bool, error) {
	if l == nil {
		return true, post(func(error) {})
	}
	l.mu.Lock()
	if seq <= l.seen || l.given[seq] || l.pending[seq] {
		l.mu.Unlock()
		return false, nil
	}
	l.pending[seq] = true
	l.mu.Unlock()
	if err := post(func(err error) { l.sent(seq, err) }); err != nil {
		l.mu.Lock()
		delete(l.pending, seq)
		l.mu.Unlock()
		return true, err
	}
	return true, nil
}

// sent records whether the session loop put turn seq into the session.
func (l *turnLedger) sent(seq int64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pending, seq)
	if err == nil {
		l.given[seq] = true
	}
}

// refusedOnce is whether seq was refused for a closed input for the
// first time: the node says so once, not at every renewal.
func (l *turnLedger) refusedOnce(seq int64) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed[seq] {
		return false
	}
	l.closed[seq] = true
	return true
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

// deliverTurn gives the lease's session the job's turn, once: from the
// event stream or a renewal's answer, whichever comes first. An
// interrupt stops the session's turn in flight first.
func (r *Runtime) deliverTurn(leaseID string, t meshapi.Turn, via string) {
	tool := r.turnTool(leaseID)
	l := r.leaseTurns(leaseID)
	posted, err := l.give(t.Seq, func(sent func(error)) error {
		// an interrupt is one message, so nothing — the interrupted turn's
		// result ending a one-shot session's last turn — comes between
		// the stop and the turn that stopped it
		return r.deliver(leaseID, Message{Kind: MsgTurn, Text: turnMessage(tool, t), Sent: sent,
			Interrupt: t.Kind == meshapi.TurnKindInterrupt})
	})
	switch {
	case errors.Is(err, errInputClosed):
		if l.refusedOnce(t.Seq) {
			r.logf("nodev1: lease %s: turn %s (seq %d, %s) came after the session's input closed; it is not the session's and stays unacked",
				leaseID, t.TurnID, t.Seq, orTurn(t.Kind))
		}
	case err != nil:
		r.logf("nodev1: turn %s for lease %s: %v", t.TurnID, leaseID, err)
	case posted:
		r.logf("nodev1: lease %s: turn %s (seq %d, %s) went to the session's mailbox, from %s", leaseID, t.TurnID, t.Seq, orTurn(t.Kind), via)
	}
}

// orTurn is a turn's kind, turn when Mesh named none.
func orTurn(kind string) string {
	if kind == "" {
		return meshapi.TurnKindTurn
	}
	return kind
}

// renewLease renews the lease, naming how far its session has the
// job's turns, and gives the session the turns the answer carries. A
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
// the node does not hold or one whose job takes no turns.
func (r *Runtime) leaseTurns(leaseID string) *turnLedger {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.lease(leaseID); l != nil && l.mailbox != nil {
		return l.mailbox.turns
	}
	return nil
}

// renewTurnsSeen is the lease's renewal's turns_seen: nil for a lease
// whose job takes no turns.
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
