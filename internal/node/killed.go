package node

import (
	"fmt"
	"strings"

	"projectmesh.io/mesh-cli/internal/harness"
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
// the session's end killed and why the kill failed, one line each, and
// returns the killed sentence for the item's note ("" when none).
func logKilled(logf func(string, ...any), subject string, res harness.Result) string {
	note := KilledNote(res.Killed)
	if note != "" {
		logf("%s: %s", subject, note)
	}
	if res.KillError != "" {
		logf("%s: kill: %s", subject, res.KillError)
	}
	return note
}

// stopFound kills a found session's unit — the one an item, plan or
// direction no longer this node's left behind — and logs what it
// killed, as a session's own end does.
func stopFound(logf func(string, ...any), subject string, s FoundSession) {
	killed, err := s.Kill()
	res := harness.Result{Killed: killed}
	if err != nil {
		res.KillError = err.Error()
	}
	logKilled(logf, subject, res)
}
