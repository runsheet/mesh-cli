package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TranscriptSource finds what a session wrote under its config dir, the
// file RestoreTranscript puts back in a fresh one.
func TestTranscriptSourceFindsWhatRestoreTranscriptPlaces(t *testing.T) {
	ws := t.TempDir()
	src := filepath.Join(t.TempDir(), "rollout-2026-10-13T10-00-00-thread-9.jsonl")
	if err := os.WriteFile(src, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, adapter := range []string{"claude-code", "codex"} {
		dir := t.TempDir()
		if _, err := TranscriptSource(dir, adapter, ws, "thread-9"); err == nil {
			t.Fatalf("%s: found a transcript in an empty dir", adapter)
		}
		if err := RestoreTranscript(dir, adapter, ws, "thread-9", src); err != nil {
			t.Fatal(err)
		}
		got, err := TranscriptSource(dir, adapter, ws, "thread-9")
		if err != nil {
			t.Fatalf("%s: %v", adapter, err)
		}
		if _, err := os.Stat(got); err != nil {
			t.Fatalf("%s: %s: %v", adapter, got, err)
		}
	}
}

func TestIsUnknownSession(t *testing.T) {
	for err, want := range map[error]bool{
		nil:                                    false,
		ErrUnknownSession:                      true,
		fmt.Errorf("x: %w", ErrUnknownSession): true,
		errors.New("harness claude-code: No conversation found with session ID: abc"): true,
		errors.New("harness claude-code: rate limited"):                               false,
	} {
		if got := IsUnknownSession(err); got != want {
			t.Errorf("IsUnknownSession(%v) = %v, want %v", err, got, want)
		}
	}
}
