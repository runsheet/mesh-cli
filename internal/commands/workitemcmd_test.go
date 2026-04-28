package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/auth"
)

// TestUpdateCurrentOwnerPositional verifies that the positional 3rd argument
// (currentOwner) is included in the POST /work-item/update request body.
func TestUpdateCurrentOwnerPositional(t *testing.T) {
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/work-item/update" {
			if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
				http.Error(w, "bad body", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":         true,
				"workItemId": "wi-abc",
				"actor":      map[string]any{"kind": "user"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// Inject a fake session pointing at the test server.
	fakeSession := &auth.Session{
		AccessToken:  "test-token",
		RefreshToken: "test-refresh",
		BaseURL:      srv.URL,
	}
	overrideSessionForTest(fakeSession)
	defer overrideSessionForTest(nil)

	rootCmd.SetArgs([]string{"work-item", "update", "wi-abc", "in_review", "larry-pa"})
	buf := &strings.Builder{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	err := rootCmd.Execute()
	rootCmd.SetArgs(nil)

	if err != nil {
		t.Fatalf("command failed: %v\noutput: %s", err, buf.String())
	}

	if capturedBody == nil {
		t.Fatal("server never received a request body")
	}

	// Status should be normalized from "in_review" → same value
	if got, _ := capturedBody["status"].(string); got != "in_review" {
		t.Errorf("status: got %q, want %q", got, "in_review")
	}

	// currentOwner from positional args must be present
	co, ok := capturedBody["currentOwner"]
	if !ok {
		t.Error("currentOwner missing from request body (positional arg not sent)")
	} else if co != "larry-pa" {
		t.Errorf("currentOwner: got %q, want %q", co, "larry-pa")
	}
}

// TestUpdateCurrentOwnerFlagPath verifies that --current-owner flag also sends
// currentOwner correctly (regression guard for the flag path).
func TestUpdateCurrentOwnerFlagPath(t *testing.T) {
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/work-item/update" {
			_ = json.NewDecoder(r.Body).Decode(&capturedBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":         true,
				"workItemId": "wi-xyz",
				"actor":      map[string]any{"kind": "user"},
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

	rootCmd.SetArgs([]string{"work-item", "update", "wi-xyz", "--current-owner", "max-pa", "--status", "in_review"})
	buf := &strings.Builder{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	err := rootCmd.Execute()
	rootCmd.SetArgs(nil)

	if err != nil {
		t.Fatalf("command failed: %v\noutput: %s", err, buf.String())
	}

	if co, ok := capturedBody["currentOwner"]; !ok || co != "max-pa" {
		t.Errorf("currentOwner via flag: got %v (present=%v), want %q", co, ok, "max-pa")
	}
}
