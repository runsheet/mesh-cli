package commands

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"testing"

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
	if _, _, _, _, err := registerNode(context.Background(), &cobra.Command{}, logger, state, check); err == nil {
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
	if _, _, _, _, err := registerNode(context.Background(), &cobra.Command{}, logger, state, node.StartCheck{}); err == nil {
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
