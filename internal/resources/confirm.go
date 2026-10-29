package resources

// The self-update's confirm and rollback (ADR-044 amendment; the legacy
// node's selfupdate.go, ported): ReleaseUpdater records each install in
// the node's update state file before it swaps the binary, the old one
// kept beside it as <exe>.prev. Every start of the new binary counts
// itself against the pending update (Confirm.Start); the first heartbeat
// Mesh answers confirms it; a start that cannot confirm within the
// window ends the process for the service manager to start again; and
// the third unconfirmed start puts <exe>.prev back and ends the process,
// so the service manager starts the previous binary. A version rolled
// back is not installed again: only a newer one is.

import (
	"errors"
	"fmt"
	"time"

	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// ErrUnconfirmed is a pending update that did not confirm within its
// window: the process ends with it, and the service manager's restart is
// the update's next start.
var ErrUnconfirmed = errors.New("update not confirmed")

// UnconfirmedError is ErrUnconfirmed with the version, its window and
// the last error that kept it from confirming.
type UnconfirmedError struct {
	Version string
	Window  time.Duration
	Last    error
}

func (e *UnconfirmedError) Error() string {
	if e.Last == nil {
		return fmt.Sprintf("%s did not confirm within %s", e.Version, e.Window)
	}
	return fmt.Sprintf("%s did not confirm within %s: %v", e.Version, e.Window, e.Last)
}
func (e *UnconfirmedError) Is(target error) bool { return target == ErrUnconfirmed }
func (e *UnconfirmedError) Unwrap() error        { return e.Last }

// Confirm is a v1 node's side of the update state: State is its file
// (the node's own, never the legacy node's ~/.mesh/update.json), Exe
// the executable the update swapped, Within the confirm window
// (selfupdate.DefaultConfirmWithin when zero).
type Confirm struct {
	State  *selfupdate.StateFile
	Exe    string
	Within time.Duration
}

// Window is the confirm window.
func (c *Confirm) Window() time.Duration {
	if c.Within > 0 {
		return c.Within
	}
	return selfupdate.DefaultConfirmWithin
}

// StartCheck is what the update state said as the node started.
type StartCheck struct {
	// RolledBack: the new binary had started MaxStarts times without
	// confirming and <exe>.prev is back in place; the process ends for
	// the service manager to start the previous binary.
	RolledBack bool
	From, To   string
	// Pending: this start is still confirming the update, its Attempts'th.
	Pending  bool
	Attempts int
	// Reason is why the update failed, when it did.
	Reason string
}

// Line is the check as the node's first log line about updates.
func (c StartCheck) Line() string {
	switch {
	case c.RolledBack:
		return fmt.Sprintf("self-update: %s did not confirm in %d starts; rolled back to %s: %s", c.To, selfupdate.MaxStarts, c.From, c.Reason)
	case c.Pending:
		return fmt.Sprintf("self-update: confirming update %s → %s: start %d of %d, rolls back on the third", c.From, c.To, c.Attempts, selfupdate.MaxStarts)
	case c.Reason != "":
		return fmt.Sprintf("self-update: the update to %s failed: %s", c.To, c.Reason)
	}
	return "self-update: no update pending"
}

// Start counts this start against a pending update and, on the
// MaxStarts'th, rolls back to <exe>.prev.
func (c *Confirm) Start() (StartCheck, error) {
	attempts, err := c.State.NoteStart()
	if err != nil {
		return StartCheck{}, err
	}
	roll, err := c.State.ShouldRollback()
	if err != nil {
		return StartCheck{}, err
	}
	if roll {
		if err := c.State.Rollback(c.Exe, "", c.Window()); err != nil {
			return StartCheck{}, err
		}
	}
	s, err := c.State.Load()
	if err != nil {
		return StartCheck{}, err
	}
	return StartCheck{RolledBack: roll, From: s.From, To: s.To, Pending: s.Pending(), Attempts: attempts, Reason: s.LastError}, nil
}

// Pending is whether an update awaits its confirm.
func (c *Confirm) Pending() bool {
	s, err := c.State.Load()
	return err == nil && s.Pending()
}

// Confirmed records the update confirmed: Mesh answered the new binary.
func (c *Confirm) Confirmed() error { return c.State.Confirm() }

// StartFailed records why this start ended before it could confirm.
func (c *Confirm) StartFailed(err error) error { return c.State.NoteStartFailure(err.Error()) }

// Failed records why a heartbeat of an unconfirmed update did not go
// through.
func (c *Confirm) Failed(err error) error { return c.State.NoteConfirmError(err.Error()) }

// Unconfirmed is the error a start that ran out its window ends with.
func (c *Confirm) Unconfirmed() error {
	s, _ := c.State.Load()
	var last error
	if s.ConfirmError != "" {
		last = errors.New(s.ConfirmError)
	}
	return &UnconfirmedError{Version: s.To, Window: c.Window(), Last: last}
}
