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

// TestProjectRolesRetired locks in the Phase 1 retirement of GET
// /project/:id/roles (mesh-api routes/project.ts:288-294 returns 410).
// The CLI subcommand still exists but should surface the API error
// rather than silently print a tabwriter against a fictional response.
// If a future change accidentally re-enables the route, this test will
// flag it.
func TestProjectRolesRetired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/roles") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":    false,
				"error": "endpoint_retired",
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
	captureStdout(func() {
		rootCmd.SetArgs([]string{"project", "roles", "proj-abc"})
		rootCmd.SetErr(io.Discard)
		cmdErr = rootCmd.Execute()
		rootCmd.SetArgs(nil)
	})

	if cmdErr == nil {
		t.Fatal("expected project roles command to fail against retired (410) endpoint, got nil error")
	}
}
