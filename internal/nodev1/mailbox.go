package nodev1

import (
	"errors"
	"fmt"
	"sync"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// Every v1 job runs in a streaming session, its input held open (Mesh v1
// spec, "Streaming sessions everywhere"; PR 2.10). A lease's Mailbox is
// how anything of the node reaches the live session: a thread's turn off
// the node's event stream, an escalation from a node tool (red,
// conflict, asked) or the session's own stall, and the product's answer
// to the finish tool, after which a one-shot job's input closes. The
// session's loop (ClaudeSession) is the mailbox's one reader. Cancelling
// is the lease's ctx, not a message: it stops the session at once.

// mailboxSize bounds the messages waiting for the session's loop.
const mailboxSize = 32

// MessageKind is what a mailbox message asks of the session.
type MessageKind string

const (
	// MsgTurn is a user turn for the session: Text.
	MsgTurn MessageKind = "turn"
	// MsgEscalate moves the session up its kind's model ladder: On is
	// the trigger (escalate.OnRed, …), Text why.
	MsgEscalate MessageKind = "escalate"
	// MsgInterrupt stops the turn in flight.
	MsgInterrupt MessageKind = "interrupt"
)

// Message is one message of a lease's mailbox.
type Message struct {
	Kind MessageKind
	Text string
	On   string
	// Interrupt stops the turn in flight before an escalation moves the
	// session, so the next turn runs on the new model: a stop rule's
	// escalation, caught mid-turn (stoprules.go). On a MsgTurn it stops
	// the turn in flight before the turn goes in: an interrupt turn
	// (turns.go).
	Interrupt bool
	// Sent, when set, hears whether a MsgTurn's text went into the
	// session: nil once it did. A message the session loop never read
	// (its input closed first) is never answered. A job's turn is acked
	// to Mesh only once Sent heard nil (turns.go).
	Sent func(error)
}

// errInputClosed refuses a turn for a session whose input has closed: the
// product answered a one-shot job's finish tool, its last turn ended, or
// a thread parked. The turn is not the session's, and is never acked.
var errInputClosed = errors.New("the session's input is closed: it takes no more turns")

// Mailbox is one lease's mailbox.
type Mailbox struct {
	ch       chan Message
	once     sync.Once
	finished chan struct{}
	// turns is the record of the turns the lease's session was given
	// (turns.go), for a lease whose brief names a turn tool or a thread;
	// nil for any other lease.
	turns *turnLedger
	// closed closes once the session loop closed the session's input.
	closeOnce sync.Once
	closed    chan struct{}
}

// NewMailbox is an empty mailbox.
func NewMailbox() *Mailbox {
	return &Mailbox{ch: make(chan Message, mailboxSize), finished: make(chan struct{}), closed: make(chan struct{})}
}

// Post leaves m for the session; a full mailbox refuses it, and so does
// one whose session takes no more input, for a turn or an interrupt
// (errInputClosed).
func (m *Mailbox) Post(msg Message) error {
	if (msg.Kind == MsgTurn || msg.Kind == MsgInterrupt) && m.InputClosed() {
		return errInputClosed
	}
	select {
	case m.ch <- msg:
		return nil
	default:
		return fmt.Errorf("the lease's mailbox is full (%d messages waiting)", mailboxSize)
	}
}

// C is the session loop's end of the mailbox.
func (m *Mailbox) C() <-chan Message { return m.ch }

// Finish says the product answered the job's finish tool: a one-shot
// job's input closes and its session ends once its turn does.
func (m *Mailbox) Finish() { m.once.Do(func() { close(m.finished) }) }

// Finished closes once Finish is called.
func (m *Mailbox) Finished() <-chan struct{} { return m.finished }

// closeInput says the session loop closed the session's input, or
// stopped reading: no turn reaches the session after it.
func (m *Mailbox) closeInput() { m.closeOnce.Do(func() { close(m.closed) }) }

// InputClosed is whether the session takes no more turns: the product
// answered the finish tool, or the session loop closed the input.
func (m *Mailbox) InputClosed() bool {
	select {
	case <-m.finished:
		return true
	case <-m.closed:
		return true
	default:
		return false
	}
}

// turnLine is the one fixed line a thread's turn becomes in the session
// (projectmesh-io/api ADR 001, "Threads carry no content"): its ids
// alone, and the session-visible tool that reads it, tool the brief's
// turn tool as turnToolFor qualified it. With no turn tool the line
// still names the turn.
func turnLine(tool, turnID string, seq int64) string {
	return turnMessage(tool, meshapi.Turn{TurnID: turnID, Seq: seq})
}

// turnMessage is what a turn becomes in the session, by its kind and
// who it is from (api ADR 001 amendment, threads: "Turns into any live
// job"), still its ids alone and the tool that reads it:
//
//   - turn: turnLine's line, saying it is from a person when it is;
//   - nudge: a user message tagged [platform nudge], saying it is the
//     platform's advice, not a person's and not an order, read with the
//     turn tool, which the session may decline (a person's nudge is
//     tagged [nudge] and says it is a person's);
//   - interrupt: the line of a turn that stopped what the session was
//     doing — the node interrupted its turn in flight first (turns.go).
func turnMessage(tool string, t meshapi.Turn) string {
	if tool == "" {
		tool = "the product's turn tool"
	}
	person := t.From == meshapi.TurnFromPerson
	from := ""
	if person {
		from = " from a person"
	}
	switch t.Kind {
	case meshapi.TurnKindNudge:
		tag, who := "[platform nudge]", "the platform, not a person"
		if person {
			tag, who = "[nudge]", "a person, as advice"
		}
		return fmt.Sprintf("%s A message from %s: turn_id %s (seq %d). Read it with `%s`; it is advice, act on it if it helps, or decline it and carry on.",
			tag, who, t.TurnID, t.Seq, tool)
	case meshapi.TurnKindInterrupt:
		return fmt.Sprintf("turn %d arrived%s and stopped what you were doing: read it with %s(%s) before you carry on", t.Seq, from, tool, t.TurnID)
	}
	return fmt.Sprintf("turn %d arrived%s: read it with %s(%s)", t.Seq, from, tool, t.TurnID)
}
