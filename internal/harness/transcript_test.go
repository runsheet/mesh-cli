package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// RestoreTranscript puts a Claude Code transcript where `claude --resume
// <id>` reads it: <configDir>/projects/<encoded-workspace>/<id>.jsonl,
// the workspace's absolute path with every non-alphanumeric byte
// replaced by '-' — a fixture path, asserted exactly.
func TestRestoreTranscriptClaudeCode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "kept.jsonl")
	if err := os.WriteFile(src, []byte(`{"type":"summary"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(dir, "config")
	workspace := "/home/anythink/.mesh/work/ship-it/1"
	sessionID := "11111111-1111-1111-1111-111111111111"

	if err := RestoreTranscript(configDir, "claude-code", workspace, sessionID, src); err != nil {
		t.Fatalf("RestoreTranscript: %v", err)
	}
	want := filepath.Join(configDir, "projects", "-home-anythink--mesh-work-ship-it-1", sessionID+".jsonl")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("transcript not at the fixture path %s: %v", want, err)
	}
	if string(got) != `{"type":"summary"}`+"\n" {
		t.Fatalf("transcript content = %q", got)
	}
}

// A Codex rollout is restored to its own self-describing name under
// <configDir>/sessions/YYYY/MM/DD/, the date its filename carries.
func TestRestoreTranscriptCodex(t *testing.T) {
	dir := t.TempDir()
	sessionID := "22222222-2222-2222-2222-222222222222"
	name := "rollout-2026-10-13T09-30-00-" + sessionID + ".jsonl"
	src := filepath.Join(dir, name)
	if err := os.WriteFile(src, []byte(`{"type":"session_meta"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(dir, "codex-home")

	if err := RestoreTranscript(configDir, "codex", "/workspace", sessionID, src); err != nil {
		t.Fatalf("RestoreTranscript: %v", err)
	}
	want := filepath.Join(configDir, "sessions", "2026", "10", "13", name)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("rollout not at %s: %v", want, err)
	}
}

// A Codex rollout file that does not name sessionID is refused.
func TestRestoreTranscriptCodexWrongSession(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "rollout-2026-10-13T09-30-00-11111111-1111-1111-1111-111111111111.jsonl")
	if err := os.WriteFile(src, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreTranscript(filepath.Join(dir, "home"), "codex", "/workspace", "22222222-2222-2222-2222-222222222222", src); err == nil {
		t.Fatal("expected an error for a rollout naming another session")
	}
}

// A missing source is an error, not a silent no-op.
func TestRestoreTranscriptMissingSource(t *testing.T) {
	dir := t.TempDir()
	err := RestoreTranscript(filepath.Join(dir, "config"), "claude-code", "/workspace", "s1", filepath.Join(dir, "missing.jsonl"))
	if err == nil {
		t.Fatal("expected an error for a missing source")
	}
}

// An unknown adapter is refused.
func TestRestoreTranscriptUnknownAdapter(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "kept.jsonl")
	if err := os.WriteFile(src, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreTranscript(filepath.Join(dir, "config"), "sonnet-cli", "/workspace", "s1", src); err == nil {
		t.Fatal("expected an error for an unknown adapter")
	}
}
