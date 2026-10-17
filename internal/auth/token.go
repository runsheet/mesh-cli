package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The node's platform credential, at ~/.mesh/token, mode 0600
// (MESH_TOKEN overrides it for one process). Two shapes live there while
// the estate moves off console tokens (latchkey adr-machine-identity,
// rollout phase 2):
//
//   - a Latchkey machine credential — the device flow's tokens as JSON in
//     the Session shape, kind "machine". The access token is refreshed
//     before it expires; the machine is the subject and the operator
//     rides beside it.
//   - an rs_* console token from Settings → Tokens — one line, the
//     person's own bearer. The fallback, until the console stops minting.

// MachineKind is Session.Kind for a Latchkey machine credential.
const MachineKind = "machine"

// RefreshSkew is how long before expiry a machine access token is
// renewed. The node talks to the gateway every interval (30s by
// default), so a minute is ample and stays honest for the short
// access_ttl a machine may be registered with.
// A session's runsheet token file is rewritten well inside it
// (node.TokenRefreshEvery).
const RefreshSkew = time.Minute

func tokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "token"), nil
}

// Credential is what ~/.mesh/token holds: exactly one of the two shapes.
type Credential struct {
	// Console is the rs_* token, empty for a machine credential.
	Console string
	// Machine is the Latchkey credential, nil for a console token.
	Machine *Session
	// FromEnv records that MESH_TOKEN supplied it: this process may
	// refresh it in memory but must not write it to disk.
	FromEnv bool
}

// IsMachine reports whether the credential is a Latchkey machine.
func (c *Credential) IsMachine() bool { return c != nil && c.Machine != nil }

// Bearer is the token as it stands, without refreshing it. Use Source
// for a token that is live.
func (c *Credential) Bearer() string {
	switch {
	case c == nil:
		return ""
	case c.Machine != nil:
		return c.Machine.AccessToken
	default:
		return c.Console
	}
}

// LoadCredential reads MESH_TOKEN, else ~/.mesh/token.
func LoadCredential() (*Credential, error) {
	if v := strings.TrimSpace(os.Getenv("MESH_TOKEN")); v != "" {
		cred, err := parseCredential(v)
		if err != nil {
			return nil, fmt.Errorf("MESH_TOKEN: %w", err)
		}
		cred.FromEnv = true
		return cred, nil
	}
	path, err := tokenPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("no platform credential: run `mesh node login` (Latchkey) or `mesh auth token <rs_...>` (Settings → Tokens on the console)")
		}
		return nil, err
	}
	cred, err := parseCredential(string(data))
	if err != nil {
		return nil, fmt.Errorf("~/.mesh/token: %w", err)
	}
	return cred, nil
}

// parseCredential tells the two shapes apart: JSON is a machine
// credential, anything else is a console token.
func parseCredential(raw string) (*Credential, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty: run `mesh node login`")
	}
	if !strings.HasPrefix(raw, "{") {
		return &Credential{Console: raw}, nil
	}
	var sess Session
	if err := json.Unmarshal([]byte(raw), &sess); err != nil {
		return nil, fmt.Errorf("not a machine credential: %w", err)
	}
	if sess.AccessToken == "" {
		return nil, errors.New("the machine credential carries no access token: run `mesh node login`")
	}
	if sess.Kind == "" {
		sess.Kind = MachineKind
	}
	return &Credential{Machine: &sess}, nil
}

// SaveToken writes an rs_* console token to ~/.mesh/token (0600).
func SaveToken(token string) error {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "rs_") {
		return fmt.Errorf("that does not look like a runsheet console token (expected rs_…); `mesh node login` with no argument signs in to Latchkey instead")
	}
	return writeToken([]byte(token + "\n"))
}

// SaveMachine writes a Latchkey machine credential to ~/.mesh/token
// (0600), replacing whatever was there.
func SaveMachine(sess *Session) error {
	if sess == nil || sess.AccessToken == "" {
		return errors.New("machine credential: no access token")
	}
	sess.Kind = MachineKind
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	return writeToken(append(data, '\n'))
}

func writeToken(data []byte) error {
	path, err := tokenPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// ClearToken removes ~/.mesh/token.
func ClearToken() error {
	path, err := tokenPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// LockToken acquires an exclusive lock on ~/.mesh/token.lock. It is the
// same discipline LockSession keeps for the mesh-api session: two mesh
// processes must not burn one refresh token, which Latchkey rotates on
// every use. The returned function releases it.
func LockToken() (release func(), err error) {
	path, err := tokenPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if lockErr := lockFile(f); lockErr != nil {
		_ = f.Close()
		return nil, lockErr
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

// NeedsRefresh reports whether a machine credential's access token has
// expired or expires within RefreshSkew. A credential with no recorded
// expiry is taken at face value.
func NeedsRefresh(sess *Session) bool {
	if sess == nil || sess.AccessToken == "" {
		return true
	}
	if sess.ExpiresAt == nil {
		return false
	}
	return time.Now().Add(RefreshSkew).After(*sess.ExpiresAt)
}

// Source is the bearer the gateway client sends on each request. A
// console token is handed back as it is; a machine credential's access
// token is refreshed before it expires — under the ~/.mesh/token lock,
// re-reading the file first, so a second mesh process that refreshed
// while this one waited is not undone.
type Source struct {
	// Refresh exchanges the credential's refresh token for new tokens.
	// It is injected (internal/latchkey has it) so this package stays
	// free of HTTP, as its doc comment promises.
	Refresh func(ctx context.Context, sess *Session) (*Session, error)

	mu   sync.Mutex
	cred *Credential
}

// NewSource returns a Source over cred, refreshing with refresh.
func NewSource(cred *Credential, refresh func(context.Context, *Session) (*Session, error)) *Source {
	return &Source{Refresh: refresh, cred: cred}
}

// Credential is the credential as this process last saw it.
func (s *Source) Credential() *Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred
}

// Token returns the bearer for the next request.
func (s *Source) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cred == nil {
		cred, err := LoadCredential()
		if err != nil {
			return "", err
		}
		s.cred = cred
	}
	if !s.cred.IsMachine() {
		return s.cred.Console, nil
	}
	if !NeedsRefresh(s.cred.Machine) {
		return s.cred.Machine.AccessToken, nil
	}
	if s.Refresh == nil || s.cred.Machine.RefreshToken == "" {
		return "", errors.New("the machine credential has expired and carries no refresh token: run `mesh node login`")
	}
	if s.cred.FromEnv {
		// MESH_TOKEN is this process's alone: refresh it in memory and
		// leave the file (and any other process's credential) untouched.
		return s.refreshLocked(ctx)
	}
	release, err := LockToken()
	if err != nil {
		return "", fmt.Errorf("lock ~/.mesh/token: %w", err)
	}
	defer release()
	// Another mesh process may have refreshed while we waited.
	if latest, err := LoadCredential(); err == nil && latest.IsMachine() {
		if !NeedsRefresh(latest.Machine) {
			s.cred = latest
			return latest.Machine.AccessToken, nil
		}
		s.cred.Machine.RefreshToken = latest.Machine.RefreshToken
	}
	tok, err := s.refreshLocked(ctx)
	if err != nil {
		return "", err
	}
	if err := SaveMachine(s.cred.Machine); err != nil {
		return "", fmt.Errorf("refreshed the machine credential but could not save it: %w", err)
	}
	return tok, nil
}

// refreshLocked runs the injected refresh and adopts its result. Caller
// holds the mutex.
func (s *Source) refreshLocked(ctx context.Context) (string, error) {
	next, err := s.Refresh(ctx, s.cred.Machine)
	if err != nil {
		return "", fmt.Errorf("refresh the machine credential: %w — run `mesh node login`", err)
	}
	if next == nil || next.AccessToken == "" {
		return "", errors.New("refresh the machine credential: no access token — run `mesh node login`")
	}
	next.Kind = MachineKind
	s.cred.Machine = next
	return next.AccessToken, nil
}
