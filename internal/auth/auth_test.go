package auth_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/auth"
)

func TestSaveLoadSession(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	sess := &auth.Session{
		AccessToken: "tok123",
		BaseURL:     "https://api.example.com",
		UserEmail:   "test@example.com",
		CreatedAt:   time.Now().UTC(),
	}
	if err := auth.SaveSession(sess); err != nil {
		t.Fatal(err)
	}

	got, err := auth.LoadSession()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "tok123" {
		t.Errorf("access_token: got %q want tok123", got.AccessToken)
	}
	if got.Kind != "user" {
		t.Errorf("kind: got %q want user", got.Kind)
	}
}

func TestClearSession(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	sess := &auth.Session{AccessToken: "tok", BaseURL: "https://x.com"}
	if err := auth.SaveSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := auth.ClearSession(); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.LoadSession(); err == nil {
		t.Error("expected error after ClearSession, got nil")
	}
	// Idempotent — calling again on a missing file should not error.
	if err := auth.ClearSession(); err != nil {
		t.Errorf("second ClearSession should be a no-op, got: %v", err)
	}
}

func TestIsSessionValid(t *testing.T) {
	if auth.IsSessionValid(nil) {
		t.Error("nil session should be invalid")
	}
	if auth.IsSessionValid(&auth.Session{}) {
		t.Error("empty access token should be invalid")
	}

	valid := &auth.Session{AccessToken: "tok"}
	if !auth.IsSessionValid(valid) {
		t.Error("non-expired session should be valid")
	}

	past := time.Now().Add(-time.Hour)
	expired := &auth.Session{AccessToken: "tok", ExpiresAt: &past}
	if auth.IsSessionValid(expired) {
		t.Error("expired session should be invalid")
	}
}

func TestSaveLoadNodeToken(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := auth.SaveNodeToken("mytoken"); err != nil {
		t.Fatal(err)
	}
	// Verify file was created.
	tokPath := filepath.Join(tmp, ".mesh", "node-token")
	if _, err := os.Stat(tokPath); err != nil {
		t.Fatalf("node-token file not created: %v", err)
	}

	got, err := auth.LoadNodeToken()
	if err != nil {
		t.Fatal(err)
	}
	if got != "mytoken" {
		t.Errorf("token: got %q want mytoken", got)
	}
}

func TestClearNodeToken(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := auth.SaveNodeToken("tok"); err != nil {
		t.Fatal(err)
	}
	if err := auth.ClearNodeToken(); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.LoadNodeToken(); err == nil {
		t.Error("expected error after ClearNodeToken, got nil")
	}
	// Idempotent.
	if err := auth.ClearNodeToken(); err != nil {
		t.Errorf("second ClearNodeToken should be a no-op, got: %v", err)
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.example.com/", "https://api.example.com"},
		{"https://api.example.com///", "https://api.example.com"},
		{"  https://api.example.com  ", "https://api.example.com"},
		{"https://api.example.com", "https://api.example.com"},
	}
	for _, c := range cases {
		got := auth.NormalizeBaseURL(c.in)
		if got != c.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
