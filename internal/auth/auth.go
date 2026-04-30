// Package auth handles persistent session and node-token I/O for the mesh CLI.
// It does not contain any HTTP logic — callers inject HTTP operations via
// callbacks where needed (e.g. RequireSession, which lives in cmd/mesh/).
package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Session is the shape of ~/.mesh-session.json.
type Session struct {
	Kind          string     `json:"kind,omitempty"`
	AccessToken   string     `json:"accessToken"`
	RefreshToken  string     `json:"refreshToken,omitempty"`
	IDToken       string     `json:"idToken,omitempty"`
	TokenType     string     `json:"tokenType,omitempty"`
	BaseURL       string     `json:"baseUrl"`
	TokenEndpoint string     `json:"tokenEndpoint,omitempty"`
	ClientID      string     `json:"clientId,omitempty"`
	UserID        string     `json:"userId,omitempty"`
	UserEmail     string     `json:"userEmail,omitempty"`
	DefaultOrg    string     `json:"defaultOrg,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
}

func sessionFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh-session.json"), nil
}

// LoadSession reads ~/.mesh-session.json and returns the parsed session.
func LoadSession() (*Session, error) {
	path, err := sessionFilePath()
	if err != nil {
		return nil, err
	}
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(file, &sess); err != nil {
		return nil, err
	}
	if sess.Kind == "" {
		sess.Kind = "user"
	}
	return &sess, nil
}

// SaveSession writes sess to ~/.mesh-session.json with mode 0600.
func SaveSession(sess *Session) error {
	path, err := sessionFilePath()
	if err != nil {
		return err
	}
	if sess.Kind == "" {
		sess.Kind = "user"
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// ClearSession removes ~/.mesh-session.json. Returns nil if the file doesn't
// exist.
func ClearSession() error {
	path, err := sessionFilePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// IsSessionValid returns true if sess has a non-empty access token that has
// not yet expired.
func IsSessionValid(sess *Session) bool {
	if sess == nil || sess.AccessToken == "" {
		return false
	}
	if sess.ExpiresAt != nil && time.Now().After(*sess.ExpiresAt) {
		return false
	}
	return true
}

// NormalizeBaseURL strips trailing slashes and whitespace from a base URL.
func NormalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// nodeTokenPath returns ~/.mesh/node-token.
func nodeTokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "node-token"), nil
}

// LoadNodeToken reads ~/.mesh/node-token and returns its trimmed content.
func LoadNodeToken() (string, error) {
	path, err := nodeTokenPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// SaveNodeToken writes token to ~/.mesh/node-token with mode 0600, creating
// the ~/.mesh directory if needed.
func SaveNodeToken(token string) error {
	path, err := nodeTokenPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.TrimSpace(token)+"\n"), 0o600)
}

// ClearNodeToken removes ~/.mesh/node-token. Returns nil if the file doesn't
// exist.
func ClearNodeToken() error {
	path, err := nodeTokenPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
