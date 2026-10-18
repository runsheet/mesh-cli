package node

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/validate"
)

// Record entry kinds (record.go's RecordEntry.Kind).
const (
	recordNote     = "note"
	recordQuestion = "question"
	recordAnswer   = "answer"
	recordCheck    = "check"
	recordHandback = "handback"
	recordTurn     = "turn"
)

// recordCheckLines is how many of a failed step's last output lines its
// check entry keeps — the twenty the retry prompt quotes.
const recordCheckLines = 20

// stateNotePrefix opens the note a session leaves saying where its work
// stands before it ends (BuildPrompt's and HandBackPrompt's `state:`).
const stateNotePrefix = "state:"

// recorder writes a build's entries to its item's record.jsonl as they
// happen (ResumeStore.AppendRecord): each turn's start and end, each
// mesh_progress note and mesh_ask question the stream carries, each
// failed validation step and a hand-back. Every text is the session's
// or the tool's own words, verbatim but for the session's own secrets
// (SessionAccount.Redact) — never an env value the node put there. A
// nil recorder, or one with no store, writes nothing; a write that
// fails is logged and the build goes on.
type recorder struct {
	store  *ResumeStore
	itemID string
	label  string
	logf   func(string, ...any)
	redact func(string) string
	now    func() time.Time

	mu sync.Mutex
	// stated and asked are this turn's: a mesh_progress note opening
	// state: made, a mesh_ask made.
	stated, asked bool
}

// recorder is the item's record writer on the resume store of its org
// (Runner.resumes); one with no store when the runner keeps none.
func (b *buildRun) newRecorder() *recorder {
	return &recorder{store: b.r.resumes(b.item.OrgID), itemID: b.item.ID, label: b.label, logf: b.r.logf, redact: b.acct.Redact, now: time.Now}
}

func (rc *recorder) append(kind, text string) {
	if rc == nil || rc.store == nil {
		return
	}
	if rc.redact != nil {
		text = rc.redact(text)
	}
	if err := rc.store.AppendRecord(rc.itemID, RecordEntry{At: rc.now().UTC(), Kind: kind, Text: text}); err != nil && rc.logf != nil {
		rc.logf("item %s: record: %v", rc.label, err)
	}
}

// turnStart is a session's start: its adapter, resumed (the session id)
// or fresh, and why it is fresh when that is known.
func (rc *recorder) turnStart(adapter, resume, why string) {
	if rc == nil {
		return
	}
	rc.mu.Lock()
	rc.stated, rc.asked = false, false
	rc.mu.Unlock()
	text := "start: " + adapter + ", fresh session"
	if resume != "" {
		text = "start: " + adapter + ", resumed session " + resume
	} else if why != "" {
		text += " (" + why + ")"
	}
	rc.append(recordTurn, text)
}

// turnEnd is a session's end: its adapter, its session id, resumed or
// fresh and how it ended; a hand-back entry when it was handed back,
// and, when it ended on a hand-back or after a question with no
// mesh_progress note opening state:, a turn entry saying so — nothing
// is refused for its absence.
func (rc *recorder) turnEnd(adapter, resume string, res harness.Result, err error) {
	if rc == nil {
		return
	}
	how := "fresh"
	if resume != "" {
		how = "resumed"
	}
	sid := res.SessionID
	if sid == "" {
		sid = resume
	}
	if sid == "" {
		sid = "unknown"
	}
	outcome := "ended without success"
	switch {
	case err != nil:
		outcome = "harness error: " + err.Error()
	case res.HandedBack:
		outcome = "handed back"
	case res.Success:
		outcome = "ended"
	}
	rc.append(recordTurn, fmt.Sprintf("end: %s session %s, %s, %s", adapter, sid, how, outcome))
	if res.HandedBack {
		text := "handed back: " + res.SpentOn
		if t := strings.TrimSpace(res.Text); t != "" {
			text += "\n" + res.Text
		}
		rc.append(recordHandback, text)
	}
	rc.mu.Lock()
	stated, asked := rc.stated, rc.asked
	rc.mu.Unlock()
	if stated {
		return
	}
	switch {
	case res.HandedBack:
		rc.append(recordTurn, "the turn ended on hand-back with no mesh_progress note opening state:")
	case asked:
		rc.append(recordTurn, "the turn ended on a question with no mesh_progress note opening state:")
	}
}

// workCall records a mesh_progress note or a mesh_ask question the
// stream carried (harness.Progress.WorkCall), its text exactly the
// call's; any other work call, or one with no text, is none.
func (rc *recorder) workCall(call *harness.WorkCall) {
	if rc == nil || call == nil {
		return
	}
	var field, kind string
	switch call.Tool {
	case "mesh_progress":
		field, kind = "note", recordNote
	case "mesh_ask":
		field, kind = "question", recordQuestion
	default:
		return
	}
	var in map[string]any
	if json.Unmarshal(call.Input, &in) != nil {
		return
	}
	text, _ := in[field].(string)
	if text == "" {
		return
	}
	rc.mu.Lock()
	if kind == recordQuestion {
		rc.asked = true
	} else if strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), stateNotePrefix) {
		rc.stated = true
	}
	rc.mu.Unlock()
	rc.append(kind, text)
}

// check records a failed validation step: its name, command and exit
// code, then its output's last twenty lines as the step printed them.
func (rc *recorder) check(step validate.StepResult) {
	if rc == nil {
		return
	}
	text := fmt.Sprintf("step %s (%s) exited %d", step.Name, step.Command, step.ExitCode)
	if kind := step.EnvironmentKind; kind != "" {
		text += " (" + string(kind) + ")"
	}
	if out := lastLines(step.Output, recordCheckLines); out != "" {
		text += "\n" + out
	}
	rc.append(recordCheck, text)
}

// answer records the answer to a question the session asked and who
// gave it, when the platform serves one on the item. The item's row
// carries no Q&A yet; this is the writer the read lands on.
func (rc *recorder) answer(question, answer, by string) {
	if rc == nil || answer == "" {
		return
	}
	text := answer
	if by != "" {
		text = by + ": " + answer
	}
	if question != "" {
		text = "to " + question + "\n" + text
	}
	rc.append(recordAnswer, text)
}

// lastLines is s's last n lines, the trailing newline left off.
func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// recordedRun runs one session of the build — its first turn, a fresh
// fallback, a retry turn — between its record's start and end entries.
// why is why a fresh session is fresh, "" when nothing says.
func (b *buildRun) recordedRun(job harness.Job, why string, run func() (harness.Result, error)) (harness.Result, error) {
	adapter := Adapter(b.r.Harness)
	b.rec.turnStart(adapter, job.Resume, why)
	res, err := run()
	b.rec.turnEnd(adapter, job.Resume, res, err)
	return res, err
}
