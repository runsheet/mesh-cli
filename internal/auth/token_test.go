package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/auth"
)

func machineCred(expiresIn time.Duration) *auth.Session {
	at := time.Now().Add(expiresIn).UTC()
	return &auth.Session{
		Kind: auth.MachineKind, AccessToken: "access-1", RefreshToken: "refresh-1",
		BaseURL: "https://auth.latchkey.id", TokenEndpoint: "https://auth.latchkey.id/oauth/token",
		ClientID: "client", MachineID: "machine-1", UserID: "operator-1", DefaultOrg: "runsheet",
		ExpiresAt: &at,
	}
}

// An rs_* console token stays a bare line and reads back as one.
func TestConsoleCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := auth.SaveToken("rs_deadbeef"); err != nil {
		t.Fatal(err)
	}
	cred, err := auth.LoadCredential()
	if err != nil {
		t.Fatal(err)
	}
	if cred.IsMachine() {
		t.Fatal("an rs_ token read back as a machine credential")
	}
	if cred.Bearer() != "rs_deadbeef" {
		t.Errorf("bearer: got %q", cred.Bearer())
	}
	if err := auth.SaveToken("lk_not_a_console_token"); err == nil {
		t.Error("SaveToken took something that is not an rs_ token")
	}
}

// A machine credential is JSON, 0600, and every field the refresh needs
// survives the round trip.
func TestMachineCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := machineCred(time.Hour)
	if err := auth.SaveMachine(want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(home, ".mesh", "token"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode: got %v want 0600", info.Mode().Perm())
	}
	cred, err := auth.LoadCredential()
	if err != nil {
		t.Fatal(err)
	}
	if !cred.IsMachine() {
		t.Fatal("the machine credential read back as a console token")
	}
	got := cred.Machine
	if got.AccessToken != "access-1" || got.RefreshToken != "refresh-1" {
		t.Errorf("tokens: %+v", got)
	}
	if got.TokenEndpoint != want.TokenEndpoint || got.ClientID != "client" {
		t.Errorf("endpoint/client: %+v", got)
	}
	if got.MachineID != "machine-1" || got.UserID != "operator-1" || got.DefaultOrg != "runsheet" {
		t.Errorf("machine/operator/org: %+v", got)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(*want.ExpiresAt) {
		t.Errorf("expiry: %v want %v", got.ExpiresAt, want.ExpiresAt)
	}
}

// With nothing saved, the message names both ways to get a credential.
func TestLoadCredentialMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := auth.LoadCredential()
	if err == nil {
		t.Fatal("expected an error with no credential saved")
	}
	for _, want := range []string{"mesh node login", "mesh auth token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// MESH_TOKEN wins over the file, in either shape.
func TestMeshTokenOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := auth.SaveToken("rs_onfile"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_TOKEN", "rs_fromenv")
	cred, err := auth.LoadCredential()
	if err != nil {
		t.Fatal(err)
	}
	if cred.Bearer() != "rs_fromenv" || !cred.FromEnv {
		t.Errorf("env credential: %+v", cred)
	}
	raw, err := json.Marshal(machineCred(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_TOKEN", string(raw))
	cred, err = auth.LoadCredential()
	if err != nil {
		t.Fatal(err)
	}
	if !cred.IsMachine() || !cred.FromEnv {
		t.Errorf("env machine credential: %+v", cred)
	}
}

func TestNeedsRefresh(t *testing.T) {
	if !auth.NeedsRefresh(nil) {
		t.Error("a missing credential needs a refresh")
	}
	if auth.NeedsRefresh(machineCred(time.Hour)) {
		t.Error("an hour of life left is not a refresh")
	}
	if !auth.NeedsRefresh(machineCred(30 * time.Second)) {
		t.Error("half a minute of life left should refresh (the skew is a minute)")
	}
	if !auth.NeedsRefresh(machineCred(-time.Minute)) {
		t.Error("an expired token needs a refresh")
	}
	if auth.NeedsRefresh(&auth.Session{AccessToken: "x"}) {
		t.Error("a credential with no recorded expiry is taken at face value")
	}
}

// A console token is handed over as it stands: there is nothing to refresh.
func TestSourceConsoleToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := auth.NewSource(&auth.Credential{Console: "rs_x"}, func(context.Context, *auth.Session) (*auth.Session, error) {
		t.Fatal("a console token must not be refreshed")
		return nil, nil
	})
	tok, err := src.Token(context.Background())
	if err != nil || tok != "rs_x" {
		t.Fatalf("token: %q, %v", tok, err)
	}
}

// The machine's access token is refreshed before it expires, the rotated
// refresh token is kept, and the new pair lands on disk for the next
// process.
func TestSourceRefreshesBeforeExpiry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := auth.SaveMachine(machineCred(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	cred, err := auth.LoadCredential()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	src := auth.NewSource(cred, func(_ context.Context, sess *auth.Session) (*auth.Session, error) {
		calls++
		if sess.RefreshToken != "refresh-1" {
			t.Errorf("refreshed with %q", sess.RefreshToken)
		}
		next := *sess
		next.AccessToken, next.RefreshToken = "access-2", "refresh-2"
		at := time.Now().Add(time.Hour).UTC()
		next.ExpiresAt = &at
		return &next, nil
	})
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "access-2" {
		t.Errorf("token: got %q want access-2", tok)
	}
	// The live token is used from here on, not refreshed again.
	if tok, err = src.Token(context.Background()); err != nil || tok != "access-2" || calls != 1 {
		t.Fatalf("second Token: %q, %v, %d refreshes", tok, err, calls)
	}
	saved, err := auth.LoadCredential()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.IsMachine() || saved.Machine.AccessToken != "access-2" || saved.Machine.RefreshToken != "refresh-2" {
		t.Errorf("~/.mesh/token after the refresh: %+v", saved.Machine)
	}
}

// Another mesh process refreshed while this one waited for the lock:
// adopt what it wrote rather than burning the rotated refresh token.
func TestSourceAdoptsAnotherProcessRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stale := machineCred(-time.Minute)
	src := auth.NewSource(&auth.Credential{Machine: stale}, func(context.Context, *auth.Session) (*auth.Session, error) {
		t.Fatal("refreshed although the file already held a live token")
		return nil, nil
	})
	fresh := machineCred(time.Hour)
	fresh.AccessToken, fresh.RefreshToken = "access-other", "refresh-other"
	if err := auth.SaveMachine(fresh); err != nil {
		t.Fatal(err)
	}
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "access-other" {
		t.Errorf("token: got %q want the one the other process wrote", tok)
	}
}

// A revoked machine cannot refresh: say so, and say what to run.
func TestSourceRefreshRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := auth.SaveMachine(machineCred(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	cred, _ := auth.LoadCredential()
	src := auth.NewSource(cred, func(context.Context, *auth.Session) (*auth.Session, error) {
		return nil, errors.New("invalid_grant: the machine was revoked")
	})
	_, err := src.Token(context.Background())
	if err == nil {
		t.Fatal("expected the refusal to surface")
	}
	if !strings.Contains(err.Error(), "revoked") || !strings.Contains(err.Error(), "mesh node login") {
		t.Errorf("error %q should carry the refusal and what to run", err)
	}
}

// A MESH_TOKEN credential is this process's alone: refreshing it must not
// write over whatever the machine's own credential is.
func TestSourceEnvCredentialIsNotWritten(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := auth.SaveToken("rs_onfile"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(machineCred(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_TOKEN", string(raw))
	cred, err := auth.LoadCredential()
	if err != nil {
		t.Fatal(err)
	}
	src := auth.NewSource(cred, func(_ context.Context, sess *auth.Session) (*auth.Session, error) {
		next := *sess
		next.AccessToken = "access-env"
		at := time.Now().Add(time.Hour).UTC()
		next.ExpiresAt = &at
		return &next, nil
	})
	tok, err := src.Token(context.Background())
	if err != nil || tok != "access-env" {
		t.Fatalf("token: %q, %v", tok, err)
	}
	onDisk, err := os.ReadFile(filepath.Join(home, ".mesh", "token"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(onDisk)) != "rs_onfile" {
		t.Errorf("~/.mesh/token was rewritten: %q", strings.TrimSpace(string(onDisk)))
	}
}
