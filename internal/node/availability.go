package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// StatusHarnessUnavailable is the heartbeat's status while the node's
// harness cannot run a session (mesh-specs trd §1 Node, §3).
const StatusHarnessUnavailable = "harness_unavailable"

// ErrHarnessUnavailable wraps a session's end that tripped the node's
// Availability: whatever held the session does not count it as a try.
var ErrHarnessUnavailable = errors.New("harness unavailable")

// emptyExitReason is why two silent sessions in a row trip the node.
const emptyExitReason = "two sessions in a row exited with no output"

// Availability is whether the node's harness can run a session at all
// (mesh-specs trd §3): tripped by the harness's own out-of-credit or
// rate-limit message, or by two sessions running that produced no
// output, and cleared by a probe that succeeds. It is the node's
// condition, not an item's: every session runner and the loop share one.
// A nil Availability is always available and notes nothing.
type Availability struct {
	mu          sync.Mutex
	unavailable bool
	reason      string
	empties     int
}

// Trip marks the harness unavailable, for reason.
func (a *Availability) Trip(reason string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unavailable, a.reason = true, reason
}

// Clear marks the harness available again and forgets the silent exits.
func (a *Availability) Clear() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unavailable, a.reason, a.empties = false, "", 0
}

// Status is whether the harness is unavailable, and why.
func (a *Availability) Status() (unavailable bool, reason string) {
	if a == nil {
		return false, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.unavailable, a.reason
}

// NoteEmptyExit counts a session that produced no output; the second in
// a row since the last NoteOK trips the harness.
func (a *Availability) NoteEmptyExit() (tripped bool, reason string) {
	if a == nil {
		return false, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.empties++
	if a.empties < 2 {
		return false, ""
	}
	a.unavailable, a.reason = true, emptyExitReason
	return true, emptyExitReason
}

// NoteOK resets the silent-exit count; an existing trip stays.
func (a *Availability) NoteOK() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.empties = 0
}

// Observe notes how one session ended: an UnavailableError trips the
// harness, an empty output counts toward two in a row, anything else
// resets the count. tripped says the session ended on the node's
// condition — the caller releases what it holds, uncounted, rather than
// failing it.
func (a *Availability) Observe(res harness.Result, err error) (tripped bool, reason string) {
	if u, ok := harness.AsUnavailable(err); ok {
		a.Trip(u.Reason)
		return true, u.Reason
	}
	if res.EmptyOutput {
		return a.NoteEmptyExit()
	}
	a.NoteOK()
	return false, ""
}

// ProbePrompt is the whole of a probe session's work.
const ProbePrompt = "Reply with the single word OK."

// ProbeBudgetUSD caps a probe session's spend.
const ProbeBudgetUSD = 0.05

// ProbeHarness is the loop's Probe (trd §3): one minimal session on h —
// a scratch directory, no repository, ProbePrompt, ProbeBudgetUSD — on
// the account a session would run on (OpenAccount). It returns nil only
// when the session completed: an unavailable harness, an empty output or
// any other error is an error.
func ProbeHarness(h harness.Harness, accounts []AccountConfig, timeout time.Duration) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		dir, err := os.MkdirTemp("", "mesh-probe-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		job := harness.Job{Kind: harness.KindProbe, Workspace: dir, Prompt: ProbePrompt, MaxBudgetUSD: ProbeBudgetUSD, Label: "harness probe", Timeout: timeout, Model: "haiku"}
		acct, err := OpenAccount(ctx, accounts)
		if err != nil {
			return err
		}
		defer acct.Cleanup()
		acct.Apply(&job)
		res, err := h.Run(ctx, job, nil)
		if u, ok := harness.AsUnavailable(err); ok {
			return fmt.Errorf("%w: %s", ErrHarnessUnavailable, acct.Redact(u.Reason))
		}
		if err != nil {
			return acct.RedactErr(err)
		}
		if res.EmptyOutput {
			return fmt.Errorf("%w: the probe produced no output", ErrHarnessUnavailable)
		}
		if !res.Success {
			return fmt.Errorf("the probe ended without success: %s", truncate(acct.Redact(res.Text), 300))
		}
		return nil
	}
}
