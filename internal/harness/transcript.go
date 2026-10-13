package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// projectSlug is Claude Code's own encoding of an absolute workspace
// path into its projects directory name: every non-alphanumeric byte
// replaced by '-' (docs.claude.com, Claude Code docs, Agent SDK
// "Sessions — Resume by ID": "<encoded-cwd> is the absolute working
// directory with every non-alphanumeric character replaced by -";
// verified via context7, /pjbrito/claude-code-docs, 2026-10-13).
func projectSlug(workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range abs {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String(), nil
}

// codexRolloutName matches a Codex rollout's own self-describing
// filename, `rollout-{YYYY-MM-DDTHH-mm-ss}-{thread_id}.jsonl` (optionally
// `_{rollout_id}` before the extension, or `.zst` compressed) —
// codex-rs/rollout/src/rollout_file_name.rs's RolloutFileName::parse;
// verified via context7, /openai/codex, 2026-10-13.
var codexRolloutName = regexp.MustCompile(`^rollout-(\d{4})-(\d{2})-(\d{2})T\d{2}-\d{2}-\d{2}-.+\.jsonl(\.zst)?$`)

// RestoreTranscript places a kept transcript (src) where a resumed
// session looks for it, under a fresh configDir — a session's own
// CLAUDE_CONFIG_DIR or CODEX_HOME, made new for every session (adr-005,
// mesh-cli ADR-024) — so a session a later one did not run can still be
// resumed: `claude --resume <sessionID>` reads
// `<configDir>/projects/<encoded-workspace>/<sessionID>.jsonl` (adapter
// "claude-code", ClaudeCode.Name); `codex exec resume <sessionID>` reads
// its rollout file by the date its own self-describing name carries,
// `<configDir>/sessions/YYYY/MM/DD/<src's own name>` — copying it there
// "fully resumes" the thread (codex-rs/core/src/thread_manager.rs,
// resume_thread_from_rollout; verified via context7, /openai/codex,
// 2026-10-13) — adapter "codex" (Codex.Name).
//
// A source that cannot be read is an error; an unreadable src, an
// unknown adapter, or (for Codex) a src whose name is not a rollout
// file naming sessionID are errors too. The destination's directory is
// made as needed.
func RestoreTranscript(configDir, adapter, workspace, sessionID, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("harness: restore transcript: %w", err)
	}
	var dest string
	switch adapter {
	case "codex":
		base := filepath.Base(src)
		m := codexRolloutName.FindStringSubmatch(base)
		if m == nil {
			return fmt.Errorf("harness: restore transcript: %s is not a codex rollout filename", base)
		}
		if sessionID != "" && !strings.Contains(base, sessionID) {
			return fmt.Errorf("harness: restore transcript: %s does not name session %s", base, sessionID)
		}
		dest = filepath.Join(configDir, "sessions", m[1], m[2], m[3], base)
	case "claude-code":
		slug, err := projectSlug(workspace)
		if err != nil {
			return fmt.Errorf("harness: restore transcript: %w", err)
		}
		dest = filepath.Join(configDir, "projects", slug, sessionID+".jsonl")
	default:
		return fmt.Errorf("harness: restore transcript: unknown adapter %q", adapter)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("harness: restore transcript: %w", err)
	}
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return fmt.Errorf("harness: restore transcript: %w", err)
	}
	return nil
}

// TranscriptPath is where a finished session's own transcript sits under
// its configDir — RestoreTranscript's destination read the other way:
// `<configDir>/projects/<encoded-workspace>/<sessionID>.jsonl` for
// "claude-code", the rollout file naming sessionID under
// `<configDir>/sessions/YYYY/MM/DD/` for "codex". "" when there is none
// (the dir gone, the session never wrote one, an unknown adapter).
func TranscriptPath(configDir, adapter, workspace, sessionID string) string {
	if configDir == "" || sessionID == "" {
		return ""
	}
	switch adapter {
	case "claude-code":
		slug, err := projectSlug(workspace)
		if err != nil {
			return ""
		}
		p := filepath.Join(configDir, "projects", slug, sessionID+".jsonl")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	case "codex":
		matches, _ := filepath.Glob(filepath.Join(configDir, "sessions", "*", "*", "*", "rollout-*.jsonl*"))
		for i := len(matches) - 1; i >= 0; i-- {
			base := filepath.Base(matches[i])
			if codexRolloutName.MatchString(base) && strings.Contains(base, sessionID) {
				return matches[i]
			}
		}
	}
	return ""
}

// TranscriptSource is where a session's own transcript lies under its
// configDir after it ran, the file RestoreTranscript puts back: Claude
// Code's `<configDir>/projects/<encoded-workspace>/<sessionID>.jsonl`, or
// Codex's rollout file naming sessionID under `<configDir>/sessions/`. An
// error when there is none.
func TranscriptSource(configDir, adapter, workspace, sessionID string) (string, error) {
	if configDir == "" || sessionID == "" {
		return "", errors.New("harness: transcript: no config dir or session id")
	}
	switch adapter {
	case "codex":
		matches, _ := filepath.Glob(filepath.Join(configDir, "sessions", "*", "*", "*", "rollout-*"+sessionID+"*.jsonl*"))
		for _, m := range matches {
			if codexRolloutName.MatchString(filepath.Base(m)) {
				return m, nil
			}
		}
		return "", fmt.Errorf("harness: transcript: no codex rollout for %s", sessionID)
	case "claude-code":
		slug, err := projectSlug(workspace)
		if err != nil {
			return "", fmt.Errorf("harness: transcript: %w", err)
		}
		p := filepath.Join(configDir, "projects", slug, sessionID+".jsonl")
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("harness: transcript: %w", err)
		}
		return p, nil
	}
	return "", fmt.Errorf("harness: transcript: unknown adapter %q", adapter)
}

// ErrUnknownSession is a resume the agent refused because it has no
// session of that id: Claude Code's `No conversation found with session
// ID`, Codex's missing rollout.
var ErrUnknownSession = errors.New("harness: no such session to resume")

// unknownSessionWords are what the agents say when a --resume names a
// session they cannot find.
var unknownSessionWords = []string{"no conversation found", "no rollout found", "thread not found", "session not found", "no such session"}

// IsUnknownSession says err is a resume refused for an unknown session:
// ErrUnknownSession, or an agent's own words for it.
func IsUnknownSession(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUnknownSession) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, w := range unknownSessionWords {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return false
}
