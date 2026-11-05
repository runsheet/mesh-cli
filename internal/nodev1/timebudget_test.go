package nodev1

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// tolerance is the slack timing tests in this file allow a timer-based
// deadline, on a loaded CI host, before it is read as late.
const tolerance = 300 * time.Millisecond

// TestTimeBudget: a budget with no extension ever granted cancels its
// context within a small tolerance of its duration, matching today's
// context.WithTimeout behavior exactly — Done closes and Err reads
// context.DeadlineExceeded, the same as a plain context.WithTimeout's
// context would for the same duration.
func TestTimeBudget(t *testing.T) {
	const d = 80 * time.Millisecond
	tb := newTimeBudget(context.Background(), d, nil)
	defer tb.Stop()

	start := time.Now()
	<-tb.Done()
	elapsed := time.Since(start)

	if elapsed < d {
		t.Fatalf("fired after %s, before its %s budget", elapsed, d)
	}
	if elapsed > d+tolerance {
		t.Fatalf("fired after %s, past its %s budget by more than %s", elapsed, d, tolerance)
	}
	if tb.Err() != context.DeadlineExceeded {
		t.Fatalf("Err() = %v, want context.DeadlineExceeded (context.WithTimeout's own)", tb.Err())
	}
	if _, ok := tb.Deadline(); !ok {
		t.Fatal("Deadline() answered no deadline, as context.WithTimeout's always does")
	}
}

// TestTimeBudgetWarnsBeforeDeadline: with no extension granted, exactly
// one Mailbox Message{Kind: MsgTurn} is posted before the deadline,
// naming the minutes remaining; a second check past the same point
// posts no second warning.
func TestTimeBudgetWarnsBeforeDeadline(t *testing.T) {
	const d = 150 * time.Millisecond
	mb := NewMailbox()
	tb := newTimeBudget(context.Background(), d, mb)
	defer tb.Stop()

	warnAt := d - warnLead(d)
	time.Sleep(warnAt + tolerance)

	var messages []Message
	drainMailbox(mb.C(), func(m Message) { messages = append(messages, m) })
	if len(messages) != 1 {
		t.Fatalf("got %d message(s) before the deadline, want exactly 1: %v", len(messages), messages)
	}
	if messages[0].Kind != MsgTurn {
		t.Fatalf("message kind = %q, want %q", messages[0].Kind, MsgTurn)
	}
	if !strings.Contains(messages[0].Text, "minute") {
		t.Fatalf("message %q does not name the minutes remaining", messages[0].Text)
	}

	// a second check past the same point, and past the deadline itself,
	// posts no second warning
	<-tb.Done()
	time.Sleep(tolerance)
	var again []Message
	drainMailbox(mb.C(), func(m Message) { again = append(again, m) })
	if len(again) != 0 {
		t.Fatalf("a second check posted %d more message(s), want none: %v", len(again), again)
	}
}

// TestSuiteStepTimeoutUnaffectedByLeaseBudget: a run_suite step's own
// fixed timeout (podStep.Run's context.WithTimeout(ctx, timeout), where
// ctx descends from the lease's time budget) still fires at its own
// duration regardless of the lease's time-budget component, unchanged
// from before this item.
func TestSuiteStepTimeoutUnaffectedByLeaseBudget(t *testing.T) {
	// a lease budget with plenty of time left, standing in for
	// podStep.Run's ctx argument
	tb := newTimeBudget(context.Background(), 10*time.Second, nil)
	defer tb.Stop()

	const stepTimeout = 50 * time.Millisecond
	sctx, cancel := context.WithTimeout(tb, stepTimeout)
	defer cancel()

	start := time.Now()
	<-sctx.Done()
	elapsed := time.Since(start)

	if elapsed < stepTimeout {
		t.Fatalf("step context fired after %s, before its own %s timeout", elapsed, stepTimeout)
	}
	if elapsed > stepTimeout+tolerance {
		t.Fatalf("step context fired after %s, past its own %s timeout by more than %s", elapsed, stepTimeout, tolerance)
	}
	if !errors.Is(sctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("step context Err() = %v, want context.DeadlineExceeded", sctx.Err())
	}
	if err := tb.Err(); err != nil {
		t.Fatalf("the lease's own budget Err() = %v, want nil: the step's timeout must not affect it", err)
	}
}
