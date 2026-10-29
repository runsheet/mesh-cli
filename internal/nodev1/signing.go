package nodev1

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LoadSigningKey reads the node's ed25519 key from path — the key its
// ToolRecords are signed with, whose public half it registers — and
// makes one (0600) when there is none.
func LoadSigningKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		seed, err := base64.StdEncoding.DecodeString(string(raw))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("nodev1: %s is not an ed25519 seed", path)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv.Seed())), 0o600); err != nil {
		return nil, err
	}
	return priv, nil
}

// PublicKey is the registration's signing_key: "ed25519:<base64>".
func PublicKey(priv ed25519.PrivateKey) string {
	return "ed25519:" + base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}
