package nodev1

import (
	"fmt"
	"sync"
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
	// escalation, caught mid-turn (stoprules.go).
	Interrupt bool
}

// Mailbox is one lease's mailbox.
type Mailbox struct {
	ch       chan Message
	once     sync.Once
	finished chan struct{}
	// turns is a thread lease's record of the turns its session was
	// given (turns.go), nil for any other lease.
	turns *turnLedger
}

// NewMailbox is an empty mailbox.
func NewMailbox() *Mailbox {
	return &Mailbox{ch: make(chan Message, mailboxSize), finished: make(chan struct{})}
}

// Post leaves m for the session; a full mailbox refuses it.
func (m *Mailbox) Post(msg Message) error {
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

// turnLine is the one fixed line a thread's turn becomes in the session
// (projectmesh-io/api ADR 001, "Threads carry no content"): its ids
// alone, and the session-visible tool that reads it, tool the brief's
// turn tool as turnToolFor qualified it. With no turn tool the line
// still names the turn.
func turnLine(tool, turnID string, seq int64) string {
	if tool == "" {
		tool = "the product's turn tool"
	}
	return fmt.Sprintf("turn %d arrived: read it with %s(%s)", seq, tool, turnID)
}
