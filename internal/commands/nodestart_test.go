package commands

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// An error registerNode hits before the loop exists — here, node.yaml
// unreadable — is recorded as why a pending update's start ended, while
// it is pending: a later rollback then names the real cause instead of
// the bare "did not start" the mini once reported with nothing in
// node.log to explain it.
func TestRegisterNodeRecordsStartFailureWhenUpdatePending(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	state := &selfupdate.StateFile{Path: filepath.Join(t.TempDir(), "update.json")}
	if err := state.BeginUpdate("72ff910", "0a04088"); err != nil {
		t.Fatal(err)
	}
	check := node.StartCheck{From: "72ff910", To: "0a04088", Pending: true, Attempts: 1}

	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	if _, _, _, _, _, err := registerNode(context.Background(), &cobra.Command{}, logger, state, check, time.Now()); err == nil {
		t.Fatal("registerNode with no node.yaml succeeded")
	}

	s, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.StartError == "" {
		t.Fatalf("StartError not recorded: %+v", s)
	}
	if !s.Pending() {
		t.Fatalf("NoteStartFailure ended the pending update: %+v", s)
	}
}

// The same failure with no update pending records nothing: there is
// nothing for a rollback to name.
func TestRegisterNodeRecordsNothingWithoutPendingUpdate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	state := &selfupdate.StateFile{Path: filepath.Join(t.TempDir(), "update.json")}

	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	if _, _, _, _, _, err := registerNode(context.Background(), &cobra.Command{}, logger, state, node.StartCheck{}, time.Now()); err == nil {
		t.Fatal("registerNode with no node.yaml succeeded")
	}

	s, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.StartError != "" {
		t.Fatalf("StartError recorded with no update pending: %+v", s)
	}
}

// confirmFake is a confirm window on a fake clock: each Sleep moves it.
func confirmFake(within time.Duration) (confirmWindow, *[]string, *bytes.Buffer) {
	start := time.Date(2026, 9, 28, 5, 53, 0, 0, time.UTC)
	now := start
	var notes []string
	var logs bytes.Buffer
	return confirmWindow{
		To: "0a04088", Started: start, Within: within, Every: 30 * time.Second,
		Note:  func(r string) error { notes = append(notes, r); return nil },
		Log:   log.New(&logs, "", 0),
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil },
	}, &notes, &logs
}

// While an update is pending, a registration that times out is retried
// inside the window, each failure logged with its error and recorded;
// the attempt that goes through ends it.
func TestConfirmWindowRetriesRegistration(t *testing.T) {
	w, notes, logs := confirmFake(5 * time.Minute)
	calls := 0
	err := w.retry(context.Background(), func(context.Context) error {
		if calls++; calls <= 4 {
			return context.DeadlineExceeded
		}
		return nil
	})
	if err != nil || calls != 5 {
		t.Fatalf("retry = %v after %d attempts", err, calls)
	}
	if len(*notes) != 4 || (*notes)[0] != "context deadline exceeded" {
		t.Fatalf("NoteConfirmError %q", *notes)
	}
	for _, want := range []string{
		"self-update: confirming 0a04088: register node: context deadline exceeded (the platform is slow); retrying for 5m0s more",
		"retrying for 3m30s more",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs lack %q:\n%s", want, logs)
		}
	}
}

// A window that elapses with registration failing throughout ends in
// ErrUnconfirmed naming the last error — not a start failure.
func TestConfirmWindowElapsesUnconfirmed(t *testing.T) {
	w, notes, _ := confirmFake(time.Minute)
	err := w.retry(context.Background(), func(context.Context) error { return context.DeadlineExceeded })
	if !errors.Is(err, node.ErrUnconfirmed) || !strings.Contains(err.Error(), "0a04088 did not confirm within 1m0s: register node: context deadline exceeded") {
		t.Fatalf("retry = %v", err)
	}
	if len(*notes) != 3 {
		t.Fatalf("NoteConfirmError %q: want one per attempt at 0s, 30s, 60s", *notes)
	}
	var logs bytes.Buffer
	if endRun(err, log.New(&logs, "", 0)) == nil {
		t.Fatal("endRun exits 0 on an unconfirmed update")
	}
	if !strings.Contains(logs.String(), "self-update: 0a04088 did not confirm within 1m0s: register node: context deadline exceeded; exiting for the service to start it again") {
		t.Fatalf("endRun logged %q", logs.String())
	}
}
