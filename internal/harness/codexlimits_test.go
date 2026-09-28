package harness

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCodexSessionFixture copies a testdata fixture into home/sessions,
// nested by date as codex-cli lays its rollout files out.
func writeCodexSessionFixture(t *testing.T, home, fixture string) {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "09", "28")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-test.jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A plan login's session file carries both windows, by window_minutes.
func TestParseCodexSessionLimits(t *testing.T) {
	home := t.TempDir()
	writeCodexSessionFixture(t, home, "codex_session_ratelimits.jsonl")
	rl, err := ParseCodexSessionLimits(home)
	if err != nil {
		t.Fatal(err)
	}
	if rl == nil {
		t.Fatal("rl is nil, want the fixture's snapshot")
	}
	if rl.Status != "allowed" {
		t.Fatalf("status %q, want allowed", rl.Status)
	}
	if rl.FiveHour.Utilization != 0.415 {
		t.Fatalf("five-hour utilization %v, want 0.415", rl.FiveHour.Utilization)
	}
	if want := time.Unix(1790694000, 0).UTC(); !rl.FiveHour.ResetsAt.Equal(want) {
		t.Fatalf("five-hour resetsAt %v, want %v", rl.FiveHour.ResetsAt, want)
	}
	if rl.SevenDay.Utilization != 0.1275 {
		t.Fatalf("seven-day utilization %v, want 0.1275", rl.SevenDay.Utilization)
	}
	if want := time.Unix(1791207000, 0).UTC(); !rl.SevenDay.ResetsAt.Equal(want) {
		t.Fatalf("seven-day resetsAt %v, want %v", rl.SevenDay.ResetsAt, want)
	}
}

// An API-key login's session file has a null rate_limits: nil, no
// error.
func TestParseCodexSessionLimitsNull(t *testing.T) {
	home := t.TempDir()
	writeCodexSessionFixture(t, home, "codex_session_ratelimits_null.jsonl")
	rl, err := ParseCodexSessionLimits(home)
	if err != nil {
		t.Fatal(err)
	}
	if rl != nil {
		t.Fatalf("rl = %+v, want nil", rl)
	}
}

// No sessions directory at all (a session that never got that far) is
// nil, no error, not a failure.
func TestParseCodexSessionLimitsNoSessions(t *testing.T) {
	rl, err := ParseCodexSessionLimits(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rl != nil {
		t.Fatalf("rl = %+v, want nil", rl)
	}
}
