package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// whatsappAccountMeta is the content of account.yaml for a WhatsApp account.
type whatsappAccountMeta struct {
	Name        string    `json:"name"`
	Mode        string    `json:"mode"`       // always "web" in slice 2
	PairedAt    time.Time `json:"paired_at"`
	PhoneNumber string    `json:"phone_number,omitempty"`
}

// WriteWhatsappAccount creates the account directory and writes account.yaml.
// It does NOT write store.db — that is done by the whatsmeow pairing handshake.
// The directory is created with mode 0700; account.yaml is mode 0600.
func WriteWhatsappAccount(name string, meta whatsappAccountMeta) error {
	dir, err := platformAccountDir("whatsapp", name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create whatsapp account dir: %w", err)
	}
	meta.Name = name
	meta.Mode = "web"
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "account.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write whatsapp account.yaml: %w", err)
	}
	return nil
}

// LoadWhatsappAccountMeta reads account.yaml for the named WhatsApp account.
func LoadWhatsappAccountMeta(name string) (whatsappAccountMeta, error) {
	dir, err := platformAccountDir("whatsapp", name)
	if err != nil {
		return whatsappAccountMeta{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "account.yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return whatsappAccountMeta{}, fmt.Errorf("whatsapp account %q not found", name)
		}
		return whatsappAccountMeta{}, err
	}
	var meta whatsappAccountMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return whatsappAccountMeta{}, fmt.Errorf("parse whatsapp account.yaml: %w", err)
	}
	return meta, nil
}

// UpdateWhatsappAccountMeta writes back account.yaml (e.g. to persist
// PhoneNumber after pairing completes).
func UpdateWhatsappAccountMeta(name string, meta whatsappAccountMeta) error {
	dir, err := platformAccountDir("whatsapp", name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "account.yaml.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "account.yaml"))
}

// ListWhatsappAccounts returns all paired WhatsApp account names.
func ListWhatsappAccounts() ([]string, error) {
	dir, err := meshDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(dir, "whatsapp-accounts")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// account.yaml must exist — guards against half-written dirs.
		if _, err := os.Stat(filepath.Join(root, e.Name(), "account.yaml")); err == nil {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// RemoveWhatsappAccount removes the account directory. Returns (false, nil) if
// not found.
func RemoveWhatsappAccount(name string) (bool, error) {
	dir, err := platformAccountDir("whatsapp", name)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return false, nil
	}
	return true, os.RemoveAll(dir)
}

// WhatsappGatewaySockPath returns the gateway.sock path for a WhatsApp account.
func WhatsappGatewaySockPath(name string) (string, error) {
	dir, err := platformAccountDir("whatsapp", name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gateway.sock"), nil
}

// WhatsappStorePath returns the store.db path for a WhatsApp account.
func WhatsappStorePath(name string) (string, error) {
	dir, err := platformAccountDir("whatsapp", name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "store.db"), nil
}

// agentReferencesWhatsappAccount returns true if any registered agent's channel
// list references the named WhatsApp account.
func agentReferencesWhatsappAccount(accountName string) (bool, string, error) {
	agents, err := ListAgents()
	if err != nil {
		return false, "", err
	}
	for _, a := range agents {
		for _, ch := range a.Channels {
			if ch.Type != "whatsapp" {
				continue
			}
			acc, _ := ch.Config["account"].(string)
			if acc == accountName {
				return true, a.Name, nil
			}
		}
	}
	return false, "", nil
}

// whatsappGatewayLive returns true if the gateway socket for the given account
// accepts a connection within ~200ms.
func whatsappGatewayLive(account string) bool {
	sockPath, err := WhatsappGatewaySockPath(account)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
