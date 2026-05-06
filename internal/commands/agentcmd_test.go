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

	"github.com/spf13/pflag"

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

// TestAgentUpdateRolesPayload verifies that `--roles pm,tester` sends a JSON
// array body (`roles: ["pm","tester"]`) per Phase 1 SCH-06.
func TestAgentUpdateRolesPayload(t *testing.T) {
	resetAgentUpdateFlags(t)

	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/update" {
			_ = json.NewDecoder(r.Body).Decode(&capturedBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":    true,
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

	_, err := executeAgent("update", "agent-abc", "--roles", "pm,tester")
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}

	if capturedBody == nil {
		t.Fatal("server never received a request body")
	}
	rolesVal, ok := capturedBody["roles"]
	if !ok {
		t.Fatal("roles key missing from request body")
	}
	rolesSlice, ok := rolesVal.([]any)
	if !ok {
		t.Fatalf("roles is not an array, got %T", rolesVal)
	}
	if len(rolesSlice) != 2 || rolesSlice[0] != "pm" || rolesSlice[1] != "tester" {
		t.Errorf("expected roles=[pm tester], got %v", rolesSlice)
	}
}

// resetAgentUpdateFlags restores the agent update flag state so that tests
// don't leak state from one run to the next.
func resetAgentUpdateFlags(t *testing.T) {
	t.Helper()
	agentUpdateRoles = nil
	agentUpdateName = ""
	agentUpdateDescription = ""
	agentUpdateSystemPrompt = ""
	agentUpdateTools = ""
	agentUpdateModel = ""
	agentUpdateProjectID = ""
	agentUpdateNodeID = ""
	agentUpdateForce = false
	// Reset Changed() state on every flag.
	agentUpdateCmd.Flags().VisitAll(func(f *pflag.Flag) {
		f.Changed = false
	})
}

// TestAgentUpdateRolesEmpty verifies that `--roles ""` (or no entries) sends
// an empty array (clear-all-roles signal), not null.
func TestAgentUpdateRolesEmpty(t *testing.T) {
	resetAgentUpdateFlags(t)

	var capturedBody map[string]any
	rawBody := &bytes.Buffer{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/update" {
			_, _ = io.Copy(rawBody, r.Body)
			_ = json.Unmarshal(rawBody.Bytes(), &capturedBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":    true,
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

	// `--roles ""` — empty entries are dropped, sending []
	_, err := executeAgent("update", "agent-abc", "--roles", "")
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}

	rolesVal, ok := capturedBody["roles"]
	if !ok {
		t.Fatal("roles key missing from request body")
	}
	rolesSlice, ok := rolesVal.([]any)
	if !ok {
		t.Fatalf("roles is not an array, got %T (raw=%s)", rolesVal, rawBody.String())
	}
	if len(rolesSlice) != 0 {
		t.Errorf("expected empty roles array (clear), got %v", rolesSlice)
	}
	// Confirm the wire payload uses [] not null
	if !strings.Contains(rawBody.String(), `"roles":[]`) {
		t.Errorf("expected literal `\"roles\":[]` in body, got %s", rawBody.String())
	}
}

// TestAgentUpdateNoRolesFlagNotSent verifies that omitting --roles entirely
// does not send a `roles` key at all (so the API leaves the column unchanged).
func TestAgentUpdateNoRolesFlagNotSent(t *testing.T) {
	resetAgentUpdateFlags(t)

	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/update" {
			_ = json.NewDecoder(r.Body).Decode(&capturedBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":    true,
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

	_, err := executeAgent("update", "agent-abc", "--name", "renamed")
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}
	if _, hasRoles := capturedBody["roles"]; hasRoles {
		t.Errorf("expected roles key absent when --roles not passed, got: %v", capturedBody)
	}
}

// TestProjectRolesTabwriter verifies that `mesh project roles` renders the
// five loop roles in tabwriter format with the correct columns. This exercises
// the projectcmd tabwriter path; the underlying API is now retired (returns
// 410) but the CLI subcommand remains until a future cleanup pass.
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
