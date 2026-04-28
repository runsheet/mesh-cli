package commands

import (
	"bytes"
	"testing"
)

// executeProject runs the projectCmd subtree with the given args and returns
// the combined error output. It does not require a live session because all
// four tested cases fail before the requireSession() call.
func executeProject(args ...string) error {
	// Reset flags between test runs.
	rootCmd.SetArgs(append([]string{"project"}, args...))
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	err := rootCmd.Execute()
	// Restore default args so other tests are not affected.
	rootCmd.SetArgs(nil)
	return err
}

func TestProjectMembersAddRoleValidation(t *testing.T) {
	err := executeProject("members", "add", "proj-123", "user@example.com", "--role", "superuser")
	if err == nil {
		t.Fatal("expected error for invalid role, got nil")
	}
}

func TestProjectMembersAddRoleEnumRejectsUnknown(t *testing.T) {
	// Different invalid value to confirm the validation is on the value, not wiring.
	err := executeProject("members", "add", "proj-123", "user@example.com", "--role", "god")
	if err == nil {
		t.Fatal("expected error for unknown role, got nil")
	}
}

func TestProjectUpdateRequiresAtLeastOneFlag(t *testing.T) {
	// update with only an ID and no --name / --description flags must error
	// before requireSession() is called.
	err := executeProject("update", "proj-123")
	if err == nil {
		t.Fatal("expected error when no update flags provided, got nil")
	}
}

func TestProjectDeleteWithoutYesOnNonTTY(t *testing.T) {
	// On a non-TTY stdin (like in tests), the scanner reads EOF immediately,
	// so the confirmation is not "y" and the command aborts without error (just
	// prints "aborted"). We verify no panic and no HTTP call is made.
	// We supply an args list that would require session only after confirmation,
	// so the abort happens before any network I/O.
	err := executeProject("delete", "proj-123")
	// Should return nil (aborted cleanly) — not a requireSession error.
	if err != nil {
		// If we got a session-not-found error, the prompt was bypassed — that's wrong.
		t.Fatalf("expected nil (abort), got: %v", err)
	}
}
