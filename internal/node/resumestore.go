package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ResumeKeepFor is how long a kept build session is worth resuming: a
// record older than this is swept whatever its item reads, since the
// session behind it has long expired (goal
// a-failed-check-returns-to-the-session-that-built-it).
const ResumeKeepFor = 7 * 24 * time.Hour

// ResumeRecord is what the node keeps of an item's last build session so
// a retry can resume it (`claude --resume <id>`, `codex exec resume
// <id>`) under the same credential and worktree: ids and paths only —
// never a token, a secret or an env value.
type ResumeRecord struct {
	ItemID string `json:"itemId"`
	// SessionID is the build session's own id (Claude Code's session_id,
	// Codex's thread id).
	SessionID string `json:"sessionId"`
	// Adapter is the harness that ran it: "claude-code" or "codex"
	// (harness.RestoreTranscript's names).
	Adapter string `json:"adapter"`
	// CredentialID is the issued credential the session ran on (adr-005),
	// "" on the machine's own login.
	CredentialID string `json:"credentialId,omitempty"`
	// Worktree is the item's checkout, ~/.mesh/work/<goal>/<seq>.
	Worktree string `json:"worktree"`
	// Transcript is the kept copy of the session's transcript, beside the
	// record under its source's own name (a Codex rollout file's name
	// carries its date, which RestoreTranscript reads); "" when none was
	// kept.
	Transcript string `json:"transcript,omitempty"`
	// KeptAt is when Keep kept it; Sweep ages records from it.
	KeptAt time.Time `json:"keptAt"`
}

// ResumeStore keeps one ResumeRecord per item under the node's run state
// dir, <Dir>/resume/<item id>/{record.json, <transcript>}. It outlives
// the process: a retry may come after a restart.
type ResumeStore struct {
	Dir string
}

// Resumes is the node's resume store: ~/.mesh/resume.
func (s RunState) Resumes() ResumeStore {
	return ResumeStore{Dir: filepath.Join(s.Dir, "resume")}
}

func (r ResumeStore) itemDir(itemID string) string {
	return filepath.Join(r.Dir, url.PathEscape(itemID))
}

func (r ResumeStore) recordPath(itemID string) string {
	return filepath.Join(r.itemDir(itemID), "record.json")
}

// Keep records rec for itemID, copying the transcript at transcriptSrc
// (the session's <workdir>.session/output.jsonl, ADR-008, or the
// adapter's own session file) beside it. A record kept before for the
// item is replaced whole. An empty transcriptSrc keeps the record alone;
// one that cannot be read is an error and nothing is kept. KeptAt is set
// to now when rec leaves it zero.
func (r ResumeStore) Keep(itemID string, rec ResumeRecord, transcriptSrc string) error {
	if itemID == "" {
		return errors.New("resume store: no item id")
	}
	if rec.SessionID == "" {
		return fmt.Errorf("resume store: item %s: no session id", itemID)
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	// Build the new record in a fresh dir and swap it in, so a reader
	// never sees a record with another session's transcript.
	tmp, err := os.MkdirTemp(r.Dir, ".keep-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	rec.ItemID = itemID
	rec.Transcript = ""
	if transcriptSrc != "" {
		name := filepath.Base(transcriptSrc)
		if err := copyFile(transcriptSrc, filepath.Join(tmp, name)); err != nil {
			return fmt.Errorf("resume store: item %s: transcript: %w", itemID, err)
		}
		rec.Transcript = filepath.Join(r.itemDir(itemID), name)
	}
	if rec.KeptAt.IsZero() {
		rec.KeptAt = time.Now()
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, "record.json"), append(data, '\n'), 0o600); err != nil {
		return err
	}
	dst := r.itemDir(itemID)
	// the record's entries (record.jsonl) outlive every session kept:
	// moved, never copied or rewritten, into the new dir
	if err := os.Rename(r.recordJSONLPath(itemID), filepath.Join(tmp, "record.jsonl")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resume store: item %s: record: %w", itemID, err)
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Lookup is the item's kept record; false when none is (or it cannot be
// read).
func (r ResumeStore) Lookup(itemID string) (ResumeRecord, bool) {
	if itemID == "" {
		return ResumeRecord{}, false
	}
	data, err := os.ReadFile(r.recordPath(itemID))
	if err != nil {
		return ResumeRecord{}, false
	}
	var rec ResumeRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.SessionID == "" {
		return ResumeRecord{}, false
	}
	return rec, true
}

// Forget removes the item's record and transcript.
func (r ResumeStore) Forget(itemID string) error {
	if itemID == "" {
		return nil
	}
	return os.RemoveAll(r.itemDir(itemID))
}

// resumeDone says an item status leaves nothing to resume: merged, or
// given up — cancelled, failed, superseded.
func resumeDone(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "merged", "cancelled", "canceled", "failed", "superseded":
		return true
	}
	return false
}

// Sweep forgets every record whose item statusOf reads merged,
// cancelled or failed (resumeDone), or that was kept more than
// ResumeKeepFor before now, and keeps the rest. A status read that
// errors leaves its record (unless it is past the age cap), as the
// start-up sweep leaves a scope it cannot judge. It returns the item ids
// forgotten; a directory that is not a readable record is removed.
func (r ResumeStore) Sweep(ctx context.Context, statusOf func(itemID string) (string, error), now time.Time) ([]string, error) {
	entries, err := os.ReadDir(r.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var forgot []string
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return forgot, err
		}
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		itemID, err := url.PathUnescape(e.Name())
		if err != nil {
			_ = os.RemoveAll(filepath.Join(r.Dir, e.Name()))
			continue
		}
		rec, ok := r.Lookup(itemID)
		forget := !ok || now.Sub(rec.KeptAt) > ResumeKeepFor
		if !forget && statusOf != nil {
			if status, err := statusOf(itemID); err == nil && resumeDone(status) {
				forget = true
			}
		}
		if forget {
			if err := r.Forget(itemID); err != nil {
				return forgot, err
			}
			forgot = append(forgot, itemID)
		}
	}
	return forgot, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
