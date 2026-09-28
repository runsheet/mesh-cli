package harness

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// KeepSessionOutput is how long a Codex session's whole event stream and
// stderr are kept under Codex.WorkRoot after the session ends: a
// Claude session's output.jsonl and stderr.log sit in
// sessionunit.Dir(job.Workspace) until harness.RemoveSession; a Codex
// session is not Outlive and has no such directory of its own
// (adr-005), so without this nothing of what Codex said survived it.
const KeepSessionOutput = 24 * time.Hour

// keptSessionsDir is kept-sessions' name under a work root.
const keptSessionsDir = "kept-sessions"

// keptSessionNote is a kept Codex session's small state.json-like note,
// beside its output.jsonl and stderr.log: enough to say what the
// session was for — never its prompt, never a secret.
type keptSessionNote struct {
	Kind        string    `json:"kind"`
	ItemID      string    `json:"itemId,omitempty"`
	GoalID      string    `json:"goalId,omitempty"`
	DirectionID string    `json:"directionId,omitempty"`
	Model       string    `json:"model,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
}

// openKeptSession makes root/kept-sessions/<name>/ fresh — its note,
// and its output.jsonl and stderr.log open for writing — so the whole
// stream survives the session (harness.RemoveSession never looks under
// root/kept-sessions: it only removes the job's own work dir). The
// caller closes out and errf.
func openKeptSession(root string, job Job, now time.Time) (dir string, out, errf *os.File, err error) {
	dir = filepath.Join(root, keptSessionsDir, keptSessionName(job, now))
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, nil, err
	}
	note := keptSessionNote{
		Kind: string(job.Kind), ItemID: job.Meta.ItemID, GoalID: job.Meta.GoalID,
		DirectionID: job.Meta.DirectionID, Model: job.Model, StartedAt: now,
	}
	if b, mErr := json.MarshalIndent(note, "", "  "); mErr == nil {
		_ = os.WriteFile(filepath.Join(dir, sessionunit.StateFile), append(b, '\n'), 0o600)
	}
	fail := func(err error) (string, *os.File, *os.File, error) {
		if out != nil {
			out.Close()
		}
		return "", nil, nil, err
	}
	if out, err = os.OpenFile(filepath.Join(dir, sessionunit.OutputFile), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); err != nil {
		return fail(err)
	}
	if errf, err = os.OpenFile(filepath.Join(dir, sessionunit.StderrFile), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); err != nil {
		return fail(err)
	}
	return dir, out, errf, nil
}

// keptSessionName is the job's label up to its first space (an
// evaluate job's "goalslug/seq evaluate" gives "goalslug/seq"), its
// slashes turned to dashes, and the unix time appended so two sessions
// of the same item never collide; the item id, or "codex", when the
// job carries no label.
func keptSessionName(job Job, now time.Time) string {
	base, _, _ := strings.Cut(job.Label, " ")
	base = strings.ReplaceAll(base, "/", "-")
	if base == "" {
		base = job.Meta.ItemID
	}
	if base == "" {
		base = "codex"
	}
	return fmt.Sprintf("%s-%d", base, now.Unix())
}

// SweepKeptSessions removes every kept session directory under
// root/kept-sessions whose note is older than KeepSessionOutput,
// leaving younger ones (a directory with no readable note, by its own
// modification time): the node calls it on each tick or each Codex
// session start, so a session's stream and stderr never outlive their
// day. A root with no kept-sessions directory is not an error.
func SweepKeptSessions(root string, now time.Time) error {
	dir := filepath.Join(root, keptSessionsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if now.Sub(keptSessionAge(path, e)) > KeepSessionOutput {
			os.RemoveAll(path)
		}
	}
	return nil
}

// keptSessionAge is when a kept session started: its note's StartedAt,
// or its directory's own modification time when the note cannot be
// read.
func keptSessionAge(dir string, e fs.DirEntry) time.Time {
	b, err := os.ReadFile(filepath.Join(dir, sessionunit.StateFile))
	if err == nil {
		var note keptSessionNote
		if json.Unmarshal(b, &note) == nil && !note.StartedAt.IsZero() {
			return note.StartedAt
		}
	}
	if info, err := e.Info(); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}
