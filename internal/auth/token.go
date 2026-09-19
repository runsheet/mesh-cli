package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The platform token: an rs_* console token from Settings → Tokens, the
// bearer the node holds as its operator (mesh-specs adr-002). Stored at
// ~/.mesh/token, mode 0600; MESH_TOKEN overrides it for one process.

func tokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "token"), nil
}

// LoadToken returns the platform token from MESH_TOKEN or ~/.mesh/token.
func LoadToken() (string, error) {
	if v := strings.TrimSpace(os.Getenv("MESH_TOKEN")); v != "" {
		return v, nil
	}
	path, err := tokenPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("no platform token: run `mesh auth token <rs_...>` (Settings → Tokens on the console)")
		}
		return "", err
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", errors.New("~/.mesh/token is empty: run `mesh auth token <rs_...>`")
	}
	return tok, nil
}

// SaveToken writes the token to ~/.mesh/token (0600).
func SaveToken(token string) error {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "rs_") {
		return fmt.Errorf("that does not look like a runsheet console token (expected rs_…)")
	}
	path, err := tokenPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token+"\n"), 0o600)
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
