package node

import (
	"os"
	"path/filepath"
	"strings"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// keepResume keeps the finished build's session in the runner's resume
// store (Runner.Resumes, else State's) — its id, adapter, credential and worktree, and
// a copy of its transcript — so a retry of the item can resume it after
// the session dir is gone. afterBuild calls it once the session's end is
// handled here, before its deferred RemoveSession; a node shutting down
// (the session left for adoption), a lost claim and a starved session
// never reach it. A Keep that fails is logged, never the item's failure.
func (r *Runner) keepResume(b *buildRun) {
	store := r.resumes()
	if store == nil {
		return
	}
	sid := b.session()
	if sid == "" {
		return
	}
	adapter := Adapter(r.Harness)
	rec := ResumeRecord{SessionID: sid, Adapter: adapter, CredentialID: b.acct.credentialID(), Worktree: b.dir}
	if err := store.Keep(b.item.ID, rec, sessionTranscript(b, adapter, sid)); err != nil {
		r.logf("item %s: resume record not kept: %v", b.label, err)
	}
}

// sessionTranscript is the transcript a resume reads: the adapter's own
// file under the session's config dir (its job's CLAUDE_CONFIG_DIR or
// CODEX_HOME, still there until the account's Cleanup), else the
// session's stream, `<workdir>.session/output.jsonl` (ADR-008) — all an
// adopted session, whose job is gone, has — else "".
func sessionTranscript(b *buildRun, adapter, sid string) string {
	if b.job != nil {
		key := "CLAUDE_CONFIG_DIR="
		if adapter == "codex" {
			key = "CODEX_HOME="
		}
		for i := len(b.job.Env) - 1; i >= 0; i-- {
			if dir, ok := strings.CutPrefix(b.job.Env[i], key); ok && dir != "" {
				if p := harness.TranscriptPath(dir, adapter, b.dir, sid); p != "" {
					return p
				}
				break
			}
		}
	}
	out := filepath.Join(sessionunit.Dir(b.dir), sessionunit.OutputFile)
	if fi, err := os.Stat(out); err == nil && fi.Mode().IsRegular() {
		return out
	}
	return ""
}
