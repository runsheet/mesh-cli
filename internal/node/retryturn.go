package node

import (
	"os"
	"path/filepath"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
)

// Why a retry after a failed evaluation did not resume the build session
// that built the item (goal a-failed-check-returns-to-the-session-that-built-it).
const (
	notResumedNoRecord     = "no record"
	notResumedAdapter      = "adapter changed"
	notResumedCredential   = "credential changed"
	notResumedWorktree     = "worktree gone"
	notResumedTranscript   = "transcript missing"
	notResumedUnknownAgent = "session not found"
)

// notResumedLine is what a retry that starts fresh says, in node.log and
// as the item's progress note.
func notResumedLine(label, reason string) string {
	return "item " + label + ": session not resumed (" + reason + "); fresh session with the failure"
}

// retryAfterEvaluation is the retry turn a claim of an item whose last
// evaluation failed gets: the clauses and review of its latest recorded
// evaluation (else the item's own evaluation prose), and the item's
// acceptance. ok is false for a first attempt, a passed evaluation, or an
// item with no evaluation to name.
func retryAfterEvaluation(item gateway.WorkItem, latest gateway.PriorAttempt) (harness.RetryTurn, bool) {
	if item.Attempts <= 0 || item.EvaluationPassed {
		return harness.RetryTurn{}, false
	}
	turn := harness.RetryTurn{Notes: strings.TrimSpace(latest.Notes), Acceptance: item.Acceptance}
	for _, c := range latest.Clauses {
		turn.Clauses = append(turn.Clauses, harness.FailedClause{Clause: c.Clause, Satisfy: c.Satisfy})
	}
	if turn.Notes == "" {
		turn.Notes = strings.TrimSpace(item.Evaluation)
	}
	if turn.Notes == "" && len(turn.Clauses) == 0 {
		return harness.RetryTurn{}, false
	}
	return turn, true
}

// freshRetryPrompt is a fresh session's prompt for a retry turn: the
// build prompt, then the same one message a resumed session gets.
func freshRetryPrompt(build string, turn harness.RetryTurn) string {
	return build + "\n\n# The last attempt failed\n\n" + harness.RetryTurnPrompt(turn)
}

// resumes is the resume store for an item of orgID: its own org's when
// Roots is laid out (ADR-034) — a ResumeStore over roots.Resumes, so an
// org never finds or keeps a record under another org's dir — else the
// runner's own (Resumes, then State's), nil when neither is set. An
// orgID Roots does not serve looks up and keeps nothing (the item would
// already have failed at its workspace, orgSpec).
func (r *Runner) resumes(orgID string) *ResumeStore {
	if r.Roots != nil {
		roots, err := r.Roots.ForOrgID(r.Scopes, orgID)
		if err != nil {
			return nil
		}
		s := ResumeStore{Dir: roots.Resumes}
		return &s
	}
	if r.Resumes != nil {
		return r.Resumes
	}
	if r.State == nil {
		return nil
	}
	s := r.State.Resumes()
	return &s
}

// noteRetry registers a retry turn for the heartbeat (Loop.NoteRetryTurn);
// a runner with no RetryTurns notes nothing.
func (r *Runner) noteRetry(itemID, sessionID string, fresh bool) func() {
	if fresh {
		sessionID = ""
	}
	return r.noteRetryTurn(itemID, sessionID)
}

// sessionConfigDir is where the session's agent keeps its sessions: the
// pod's CLAUDE_CONFIG_DIR for a sandboxed Claude session, else the
// account's own CLAUDE_CONFIG_DIR or CODEX_HOME; "" when the session has
// none of its own.
func sessionConfigDir(acct *SessionAccount, pod *SessionPod, adapter string) string {
	if adapter == AdapterClaudeCode && pod != nil && pod.Session != nil {
		return pod.Session.ClaudeConfigDir()
	}
	if acct == nil || acct.AccountSession == nil {
		return ""
	}
	return acct.ConfigDir
}

// resumable is the kept record a retry resumes, its transcript restored
// into the new session's config dir, or why it cannot be: no record on
// this node, another adapter, another credential, a worktree that is not
// this claim's checkout (ADR-028: one worktree per item), or a transcript
// that does not restore.
func (r *Runner) resumable(orgID, itemID, label, dir string, acct *SessionAccount, pod *SessionPod) (ResumeRecord, string) {
	store := r.resumes(orgID)
	if store == nil {
		return ResumeRecord{}, notResumedNoRecord
	}
	rec, ok := store.Lookup(itemID)
	if !ok {
		return ResumeRecord{}, notResumedNoRecord
	}
	adapter := Adapter(r.Harness)
	if rec.Adapter != adapter {
		return rec, notResumedAdapter
	}
	if rec.CredentialID != acct.CredentialID() {
		return rec, notResumedCredential
	}
	if !sameDir(rec.Worktree, dir) {
		return rec, notResumedWorktree
	}
	if _, err := os.Stat(rec.Worktree); err != nil {
		return rec, notResumedWorktree
	}
	configDir := sessionConfigDir(acct, pod, adapter)
	if configDir == "" {
		// the agent's own store, which the session wrote to itself
		return rec, ""
	}
	if rec.Transcript == "" {
		return rec, notResumedTranscript
	}
	if err := harness.RestoreTranscript(configDir, adapter, dir, rec.SessionID, rec.Transcript); err != nil {
		r.logf("item %s: %v", label, err)
		return rec, notResumedTranscript
	}
	return rec, ""
}

func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
