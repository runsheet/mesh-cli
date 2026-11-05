package nodev1

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// A lease's time budget (b.Harness.Budget.Minutes) is its session's
// wall-clock allowance. Never extended, it cancels the session's
// context exactly as today's context.WithTimeout does: Done closes
// and Err reads context.DeadlineExceeded when the budget runs out,
// propagating the lease's own ctx the same way a derived context
// always has. It is also extendable — a node tool a later item wires
// for "ask for more time" calls Extend on the one context the session
// already runs under, rather than handing the session a new one — and
// it warns the lease's mailbox once, a short while before the
// deadline, naming the minutes left, so the session can wrap up or
// ask for more time before the lease ends it; the same point checked
// again later sends no second warning.

// timeBudget is a lease's extendable deadline, itself a context.Context
// derived from the lease's own ctx.
type timeBudget struct {
	parent context.Context
	mb     *Mailbox

	mu        sync.Mutex
	deadline  time.Time
	timer     *time.Timer
	warnTimer *time.Timer
	fired     bool
	warned    bool
	closed    bool

	done chan struct{}
}

// newTimeBudget is a timeBudget of d running out from now, deriving its
// Context from parent. mb hears the one warning before the deadline
// fires; nil posts none.
func newTimeBudget(parent context.Context, d time.Duration, mb *Mailbox) *timeBudget {
	b := &timeBudget{parent: parent, mb: mb, deadline: time.Now().Add(d), done: make(chan struct{})}
	b.timer = time.AfterFunc(d, b.fire)
	if warnAt := d - warnLead(d); warnAt > 0 {
		b.warnTimer = time.AfterFunc(warnAt, b.warn)
	}
	go b.watchParent()
	return b
}

// warnLead is how long before the deadline the one warning fires: a
// tenth of the budget's own length, so a short budget still warns with
// room to notice and a long one gives real notice without crowding the
// deadline, clamped to [1s, 10m] and never past half the budget itself.
func warnLead(d time.Duration) time.Duration {
	lead := d / 10
	switch {
	case lead < time.Second:
		lead = time.Second
	case lead > 10*time.Minute:
		lead = 10 * time.Minute
	}
	if lead >= d {
		lead = d / 2
	}
	return lead
}

// Deadline implements context.Context.
func (b *timeBudget) Deadline() (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deadline, true
}

// Done implements context.Context.
func (b *timeBudget) Done() <-chan struct{} { return b.done }

// Value implements context.Context.
func (b *timeBudget) Value(key any) any { return b.parent.Value(key) }

// Err implements context.Context: context.DeadlineExceeded once the
// budget runs out, the parent's own error when that ended first, else
// context.Canceled once Stop ends it early — the same contract
// context.WithTimeout's context keeps.
func (b *timeBudget) Err() error {
	select {
	case <-b.done:
	default:
		return nil
	}
	b.mu.Lock()
	fired := b.fired
	b.mu.Unlock()
	if fired {
		return context.DeadlineExceeded
	}
	if err := b.parent.Err(); err != nil {
		return err
	}
	return context.Canceled
}

// Extend pushes the deadline out by d, as a node tool asking for more
// time will. A budget already done — fired, or stopped by its
// session — is never extended, and reports false. The warning may
// fire again once the new deadline is close.
func (b *timeBudget) Extend(d time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.deadline = b.deadline.Add(d)
	remaining := time.Until(b.deadline)
	b.timer.Reset(remaining)
	b.warned = false
	if b.warnTimer != nil {
		b.warnTimer.Stop()
	}
	b.warnTimer = nil
	if warnAt := remaining - warnLead(remaining); warnAt > 0 {
		b.warnTimer = time.AfterFunc(warnAt, b.warn)
	}
	return true
}

// Stop releases the budget's timers once its session is done with it,
// as context.WithTimeout's own cancel func does; it does not mark the
// budget as fired, so a session that finished within its time keeps
// Err reading context.Canceled rather than context.DeadlineExceeded.
func (b *timeBudget) Stop() {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
	}
	if b.warnTimer != nil {
		b.warnTimer.Stop()
	}
	b.closeLocked(false)
	b.mu.Unlock()
}

// fire is the timer's callback: the budget ran out.
func (b *timeBudget) fire() {
	b.mu.Lock()
	b.closeLocked(true)
	b.mu.Unlock()
}

// closeLocked closes done once, recording whether it closed because
// the budget fired. mu is held.
func (b *timeBudget) closeLocked(fired bool) {
	if b.closed {
		return
	}
	b.closed = true
	b.fired = fired
	close(b.done)
}

// watchParent closes the budget's context when the parent's own ends
// first, so it propagates a cancellation the way any derived context
// does.
func (b *timeBudget) watchParent() {
	select {
	case <-b.parent.Done():
		b.mu.Lock()
		b.closeLocked(false)
		b.mu.Unlock()
	case <-b.done:
	}
}

// warn posts the mailbox's one message naming the minutes left, unless
// the budget already warned or is done.
func (b *timeBudget) warn() {
	b.mu.Lock()
	if b.warned || b.closed {
		b.mu.Unlock()
		return
	}
	b.warned = true
	deadline := b.deadline
	b.mu.Unlock()

	if b.mb == nil {
		return
	}
	minutes := int(time.Until(deadline).Round(time.Minute) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	text := fmt.Sprintf("[platform] the session's time budget runs out in about %d minute(s); ask for more time if you need it", minutes)
	_ = b.mb.Post(Message{Kind: MsgTurn, Text: text})
}
