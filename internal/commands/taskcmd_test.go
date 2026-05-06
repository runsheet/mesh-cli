package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"projectmesh.io/mesh-cli/internal/auth"
)

// resetTaskCreateFlags restores task create flag state between tests so
// package-level cobra vars don't leak across test runs.
func resetTaskCreateFlags(t *testing.T) {
	t.Helper()
	createTaskOrg = ""
	createTaskWorkflowID = ""
	createTaskFirstStepInputs = ""
	createCmd.Flags().VisitAll(func(f *pflag.Flag) {
		f.Changed = false
	})
}

// TestTaskCreateV1Payload verifies that `mesh task create` posts the v1
// payload shape: { title, workflowId, firstStepInputs } per Phase 1 SCH-12.
func TestTaskCreateV1Payload(t *testing.T) {
	resetTaskCreateFlags(t)

	var capturedBody map[string]any

	// Mirror the v1 API envelope from mesh-api/src/routes/task.ts:
	// { ok: true, task: { id, title, workflowId, ... }, workflowRun: {...},
	//   stepRun: {...}, actor: {...} }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/task/create" {
			if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
				http.Error(w, "bad body", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"task": map[string]any{
					"id":              "task-abc",
					"title":           "Investigate deploy flake",
					"workflowId":      "11111111-1111-1111-1111-111111111111",
					"firstStepInputs": map[string]any{"company": "acme"},
					"projectId":       nil,
					"status":          "pending",
					"createdAt":       "2026-05-07T00:00:00Z",
				},
				"workflowRun": map[string]any{"id": "run-1", "status": "pending"},
				"stepRun":     map[string]any{"id": "step-1", "stepNodeId": "n1", "status": "pending"},
				"actor":       map[string]any{"kind": "user", "userId": "uid-1"},
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

	rootCmd.SetArgs([]string{"task", "create", "Investigate deploy flake",
		"--workflow-id", "11111111-1111-1111-1111-111111111111",
		"--first-step-inputs", `{"company":"acme"}`})
	buf := &strings.Builder{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	// printJSON writes to os.Stdout, not cmd.OutOrStdout, so capture stdout.
	var execErr error
	stdout := captureStdout(func() {
		execErr = rootCmd.Execute()
	})
	rootCmd.SetArgs(nil)

	if execErr != nil {
		t.Fatalf("command failed: %v\noutput: %s", execErr, buf.String())
	}

	if capturedBody == nil {
		t.Fatal("server never received a request body")
	}

	if got, _ := capturedBody["title"].(string); got != "Investigate deploy flake" {
		t.Errorf("title: got %q, want %q", got, "Investigate deploy flake")
	}
	if got, _ := capturedBody["workflowId"].(string); got != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("workflowId: got %q, want UUID", got)
	}
	fsi, ok := capturedBody["firstStepInputs"].(map[string]any)
	if !ok {
		t.Fatalf("firstStepInputs not an object, got: %T", capturedBody["firstStepInputs"])
	}
	if fsi["company"] != "acme" {
		t.Errorf("firstStepInputs.company: got %v, want acme", fsi["company"])
	}

	// Lock in the v1 wire contract: response envelope contains
	// task.id, not a flat taskId. Decode the printed JSON and assert.
	var printed map[string]any
	if err := json.Unmarshal([]byte(stdout), &printed); err != nil {
		t.Fatalf("printed output is not JSON: %v\noutput: %s", err, stdout)
	}
	task, ok := printed["task"].(map[string]any)
	if !ok {
		t.Fatalf("response.task is not an object, got: %T (full: %s)", printed["task"], stdout)
	}
	if id, _ := task["id"].(string); id != "task-abc" {
		t.Errorf("response.task.id: got %v, want task-abc", task["id"])
	}
}

// TestTaskCreateRequiresWorkflowID asserts the --workflow-id flag is required.
func TestTaskCreateRequiresWorkflowID(t *testing.T) {
	resetTaskCreateFlags(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "should not be called", 500)
	}))
	defer srv.Close()

	fakeSession := &auth.Session{
		AccessToken:  "test-token",
		RefreshToken: "test-refresh",
		BaseURL:      srv.URL,
	}
	overrideSessionForTest(fakeSession)
	defer overrideSessionForTest(nil)

	rootCmd.SetArgs([]string{"task", "create", "Some title"})
	buf := &strings.Builder{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	err := rootCmd.Execute()
	rootCmd.SetArgs(nil)

	if err == nil {
		t.Fatal("expected error when --workflow-id is missing, got nil")
	}
	if !strings.Contains(err.Error(), "workflow-id") {
		t.Errorf("expected error to mention workflow-id, got: %v", err)
	}
}

// TestTaskCreateInvalidFirstStepInputs asserts that --first-step-inputs must
// parse as JSON.
func TestTaskCreateInvalidFirstStepInputs(t *testing.T) {
	resetTaskCreateFlags(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "should not be called", 500)
	}))
	defer srv.Close()

	fakeSession := &auth.Session{
		AccessToken:  "test-token",
		RefreshToken: "test-refresh",
		BaseURL:      srv.URL,
	}
	overrideSessionForTest(fakeSession)
	defer overrideSessionForTest(nil)

	rootCmd.SetArgs([]string{"task", "create", "Some title",
		"--workflow-id", "11111111-1111-1111-1111-111111111111",
		"--first-step-inputs", "not-json"})
	buf := &strings.Builder{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	err := rootCmd.Execute()
	rootCmd.SetArgs(nil)

	if err == nil {
		t.Fatal("expected error for invalid --first-step-inputs JSON, got nil")
	}
	if !strings.Contains(err.Error(), "first-step-inputs") {
		t.Errorf("expected error to mention first-step-inputs, got: %v", err)
	}
}
