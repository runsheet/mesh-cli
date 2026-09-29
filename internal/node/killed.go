package node

import (
	"fmt"
	"strings"
	"sync"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// KilledNote is the sentence for what a session's end killed in its
// unit (ADR 008, The session's end) — a leak the agent should have
// cleaned up: `session end killed 2 process(es) the agent left
// running: 4242 go, 4250 runsheet`. Empty when it killed nothing.
func KilledNote(killed []sessionunit.Process) string {
	if len(killed) == 0 {
		return ""
	}
	names := make([]string, len(killed))
	for i, p := range killed {
		names[i] = fmt.Sprintf("%d %s", p.PID, p.Comm)
	}
	return fmt.Sprintf("session end killed %d process(es) the agent left running: %s", len(killed), strings.Join(names, ", "))
}

// logKilled logs, under subject ("item <label>", "goal <slug>", …), what
// the session's end killed — perSession is Result.KilledPerSession, a
// line per session, so a handed-back one's two runs are two lines — and
// why the kill failed (Result.KillError), and returns the killed
// sentences for the item's note, a line each ("" when none).
func logKilled(logf func(string, ...any), subject string, perSession [][]sessionunit.Process, killError string) string {
	var notes []string
	for _, killed := range perSession {
		if note := KilledNote(killed); note != "" {
			logf("%s: %s", subject, note)
			notes = append(notes, note)
		}
	}
	if killError != "" {
		logf("%s: kill: %s", subject, killError)
	}
	return strings.Join(notes, "\n")
}

// stopFound kills a found session's unit — the one an item, plan or
// direction no longer this node's left behind — and logs what it
// killed, as a session's own end does.
func stopFound(logf func(string, ...any), subject string, s FoundSession) {
	killed, err := s.Kill()
	killError := ""
	if err != nil {
		killError = err.Error()
	}
	logKilled(logf, subject, [][]sessionunit.Process{killed}, killError)
}

// LeftRunning holds the killed sentences of an item's evaluations that
// recorded no verdict — a timeout, an error, no verdict, no context —
// until one does: the platform takes no progress note on an item in
// evaluating, so the next verdict's notes carry them. A nil one holds
// nothing.
type LeftRunning struct {
	mu    sync.Mutex
	notes map[string][]string
}

// Hold adds note ("" adds nothing) to what is held for the item and
// returns everything held, one sentence a line.
func (l *LeftRunning) Hold(itemID, note string) string {
	if l == nil {
		return note
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if note != "" {
		if l.notes == nil {
			l.notes = map[string][]string{}
		}
		l.notes[itemID] = append(l.notes[itemID], note)
	}
	return strings.Join(l.notes[itemID], "\n")
}

// Forget drops what is held for the item, once a verdict carried it.
func (l *LeftRunning) Forget(itemID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.notes, itemID)
}
