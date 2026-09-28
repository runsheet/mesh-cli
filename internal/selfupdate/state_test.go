package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStateFile(t *testing.T) *StateFile {
	t.Helper()
	at := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	return &StateFile{Path: filepath.Join(t.TempDir(), ".mesh", "update.json"), now: func() time.Time { return at }}
}

func mustRollback(t *testing.T, f *StateFile) bool {
	t.Helper()
	ok, err := f.ShouldRollback()
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func start(t *testing.T, f *StateFile) int {
	t.Helper()
	n, err := f.NoteStart()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStateNoUpdate(t *testing.T) {
	f := newStateFile(t)
	if n := start(t, f); n != 0 {
		t.Errorf("NoteStart with no update = %d", n)
	}
	if mustRollback(t, f) {
		t.Error("ShouldRollback with no update")
	}
}

func TestStateTwoFailedStartsRollBack(t *testing.T) {
	f := newStateFile(t)
	if err := f.BeginUpdate("v1.0.0", "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	// First start of the new binary, which dies before confirming.
	if n := start(t, f); n != 1 || mustRollback(t, f) {
		t.Fatalf("start 1: attempts %d", n)
	}
	// Restarted; dies again.
	if n := start(t, f); n != 2 || mustRollback(t, f) {
		t.Fatalf("start 2: attempts %d", n)
	}
	// Third start: it failed to start twice.
	if n := start(t, f); n != 3 || !mustRollback(t, f) {
		t.Fatalf("start 3: attempts %d, want rollback", n)
	}
}

func TestStateConfirmResets(t *testing.T) {
	f := newStateFile(t)
	if err := f.BeginUpdate("v1.0.0", "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	start(t, f)
	start(t, f)
	if err := f.Confirm(); err != nil {
		t.Fatal(err)
	}
	s, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Pending() || s.Attempts != 0 || s.ConfirmedAt == nil || s.To != "v1.1.0" {
		t.Fatalf("after Confirm: %+v", s)
	}
	// Later starts of a confirmed binary count nothing.
	for range 3 {
		if n := start(t, f); n != 0 {
			t.Fatalf("NoteStart after Confirm = %d", n)
		}
	}
	if mustRollback(t, f) {
		t.Error("ShouldRollback after Confirm")
	}
}

func TestStateRollbackRestoresPrev(t *testing.T) {
	f := newStateFile(t)
	exe := filepath.Join(t.TempDir(), "mesh")
	writeExe(t, exe, "v1.0.0")
	if err := f.BeginUpdate("v1.0.0", "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := Swap(exe, []byte("v1.1.0")); err != nil {
		t.Fatal(err)
	}
	for range MaxStarts {
		start(t, f)
	}
	if !mustRollback(t, f) {
		t.Fatal("want rollback")
	}
	if err := f.Rollback(exe, "", DefaultConfirmWithin); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := readFile(t, exe); got != "v1.0.0" {
		t.Errorf("exe = %q, want v1.0.0", got)
	}
	if _, err := os.Stat(exe + ".prev"); !os.IsNotExist(err) {
		t.Error(".prev still there after rollback")
	}
	s, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Pending() || s.RolledBackAt == nil || s.LastError != "v1.1.0 did not start; rolled back to v1.0.0" {
		t.Fatalf("after Rollback: %+v", s)
	}
	if mustRollback(t, f) || start(t, f) != 0 {
		t.Error("the rolled-back binary is still counting starts")
	}
}

func TestStateRollbackWithoutPrev(t *testing.T) {
	f := newStateFile(t)
	exe := filepath.Join(t.TempDir(), "mesh")
	writeExe(t, exe, "v1.1.0")
	if err := f.BeginUpdate("v1.0.0", "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := f.Rollback(exe, "x", DefaultConfirmWithin); err == nil {
		t.Fatal("Rollback with no .prev succeeded")
	}
	if got := readFile(t, exe); got != "v1.1.0" {
		t.Errorf("exe = %q", got)
	}
}

func TestStateStartFailureRollsBackWithReason(t *testing.T) {
	f := newStateFile(t)
	exe := filepath.Join(t.TempDir(), "mesh")
	writeExe(t, exe, "72ff910")
	if err := f.BeginUpdate("72ff910", "0a04088"); err != nil {
		t.Fatal(err)
	}
	if err := Swap(exe, []byte("0a04088")); err != nil {
		t.Fatal(err)
	}
	const why = "exit status 1: load config: open node.yaml: permission denied"
	for range MaxStarts {
		start(t, f)
		if err := f.NoteStartFailure(why); err != nil {
			t.Fatal(err)
		}
		if !f.mustPending(t) {
			t.Fatal("NoteStartFailure ended the pending update")
		}
	}
	if !mustRollback(t, f) {
		t.Fatal("want rollback")
	}
	if err := f.Rollback(exe, "", DefaultConfirmWithin); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	s, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	const want = "0a04088 did not start: exit status 1: load config"
	if s.RolledBackAt == nil || !strings.Contains(s.LastError, want) {
		t.Fatalf("after Rollback: %+v, want lastError containing %q", s, want)
	}
}

func TestStateConfirmErrorRollsBackWithReason(t *testing.T) {
	f := newStateFile(t)
	exe := filepath.Join(t.TempDir(), "mesh")
	writeExe(t, exe, "72ff910")
	if err := f.BeginUpdate("72ff910", "0a04088"); err != nil {
		t.Fatal(err)
	}
	if err := Swap(exe, []byte("0a04088")); err != nil {
		t.Fatal(err)
	}
	const why = "heartbeat: context deadline exceeded"
	if err := f.NoteConfirmError(why); err != nil {
		t.Fatal(err)
	}
	if !f.mustPending(t) {
		t.Fatal("NoteConfirmError ended the pending update")
	}
	if err := f.Rollback(exe, "", 5*time.Minute); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	s, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.RolledBackAt == nil || !strings.Contains(s.LastError, "did not confirm within") || !strings.Contains(s.LastError, why) {
		t.Fatalf("after Rollback: %+v", s)
	}
}

func TestStateConfirmClearsStartAndConfirmErrors(t *testing.T) {
	f := newStateFile(t)
	if err := f.BeginUpdate("v1.0.0", "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := f.NoteStartFailure("exit status 1"); err != nil {
		t.Fatal(err)
	}
	if err := f.NoteConfirmError("heartbeat: context deadline exceeded"); err != nil {
		t.Fatal(err)
	}
	if !f.mustPending(t) {
		t.Fatal("Note calls ended the pending update")
	}
	if err := f.Confirm(); err != nil {
		t.Fatal(err)
	}
	s, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.StartError != "" || s.ConfirmError != "" {
		t.Fatalf("after Confirm: %+v", s)
	}
}

func (f *StateFile) mustPending(t *testing.T) bool {
	t.Helper()
	s, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	return s.Pending()
}

func TestStateRefuseRecordsWhy(t *testing.T) {
	f := newStateFile(t)
	if err := f.Refuse("v1.0.0", "v1.1.0", "SHA256SUMS signature does not verify"); err != nil {
		t.Fatal(err)
	}
	s, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Pending() || s.LastError != "SHA256SUMS signature does not verify" {
		t.Fatalf("after Refuse: %+v", s)
	}
	b, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"from\": \"v1.0.0\",\n  \"to\": \"v1.1.0\",\n  \"attempts\": 0,\n  \"lastError\": \"SHA256SUMS signature does not verify\"\n}\n"
	if string(b) != want {
		t.Errorf("update.json =\n%s\nwant\n%s", b, want)
	}
}
