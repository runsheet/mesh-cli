package commands

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/auth"
)

// captureStdout redirects os.Stdout to a pipe, runs fn, then restores and
// returns what was written.
func captureStdout(fn func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

// executeAgent runs the agentCmd subtree with the given args and returns any error.
func executeAgent(args ...string) (string, error) {
	rootCmd.SetArgs(append([]string{"agent"}, args...))
	buf := &strings.Builder{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	err := rootCmd.Execute()
	rootCmd.SetArgs(nil)
	return buf.String(), err
}

// TestAgentUpdateInvalidRole verifies that an unrecognised --role value is
// rejected client-side before any network call is made.
func TestAgentUpdateInvalidRole(t *testing.T) {
	// A minimal server so requireSession() can be satisfied; the role check
	// fires before any API call, so the server is never hit.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	fakeSession := &auth.Session{
		AccessToken:  "test-token",
		RefreshToken: "test-refresh",
		BaseURL:      srv.URL,
	}
	overrideSessionForTest(fakeSession)
	defer overrideSessionForTest(nil)

	_, err := executeAgent("update", "agent-abc", "--role", "wizard")
	if err == nil {
		t.Fatal("expected error for invalid --role, got nil")
	}
	if !strings.Contains(err.Error(), "invalid --role") {
		t.Errorf("expected 'invalid --role' in error, got: %v", err)
	}
}

// TestAgentUpdateRoleClear verifies that --role "" sends role: "" in the
// request body so the API can null-out the role column.
func TestAgentUpdateRoleClear(t *testing.T) {
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/update" {
			_ = json.NewDecoder(r.Body).Decode(&capturedBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"agent": map[string]any{"id": "agent-abc", "name": "my-agent"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	fakeSession := &auth.Session{
		AccessToken:  "test-token",
		RefreshToken: "test-refresh",
		BaseURL:      srv.URL,
	}
	overrideSessionForTest(fakeSession)
	defer overrideSessionForTest(nil)

	_, err := executeAgent("update", "agent-abc", "--role", "")
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}

	if capturedBody == nil {
		t.Fatal("server never received a request body")
	}
	roleVal, ok := capturedBody["role"]
	if !ok {
		t.Fatal("role key missing from request body")
	}
	if roleVal != "" {
		t.Errorf("expected role to be empty string (clear signal), got %q", roleVal)
	}
}

// TestProjectRolesTabwriter verifies that `mesh project roles` renders the
// five loop roles in tabwriter format with the correct columns.
func TestProjectRolesTabwriter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/roles") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":        true,
				"projectId": "proj-abc",
				"roles": map[string]any{
					"pm":         map[string]any{"agentId": "agent-pm", "agentName": "ralph-pm", "nodeId": "node-1"},
					"researcher": nil,
					"devlead":    map[string]any{"agentId": "agent-dev", "agentName": "ralph-dev", "nodeId": ""},
					"builder":    nil,
					"tester":     nil,
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	fakeSession := &auth.Session{
		AccessToken:  "test-token",
		RefreshToken: "test-refresh",
		BaseURL:      srv.URL,
	}
	overrideSessionForTest(fakeSession)
	defer overrideSessionForTest(nil)

	var cmdErr error
	out := captureStdout(func() {
		rootCmd.SetArgs([]string{"project", "roles", "proj-abc"})
		rootCmd.SetErr(io.Discard)
		cmdErr = rootCmd.Execute()
		rootCmd.SetArgs(nil)
	})

	if cmdErr != nil {
		t.Fatalf("project roles command failed: %v", cmdErr)
	}

	// Header row
	if !strings.Contains(out, "ROLE") || !strings.Contains(out, "AGENT NAME") || !strings.Contains(out, "AGENT ID") {
		t.Errorf("output missing expected header columns, got:\n%s", out)
	}
	// Assigned rows
	if !strings.Contains(out, "pm") || !strings.Contains(out, "ralph-pm") {
		t.Errorf("output missing pm agent row, got:\n%s", out)
	}
	// Unassigned row
	if !strings.Contains(out, "(unassigned)") {
		t.Errorf("output missing (unassigned) marker, got:\n%s", out)
	}
}
