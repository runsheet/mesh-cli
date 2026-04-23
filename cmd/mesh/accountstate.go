package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// platformAccountDir returns ~/.mesh/<platform>-accounts/<name>/
func platformAccountDir(platform, name string) (string, error) {
	dir, err := meshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, platform+"-accounts", name), nil
}

// discordAccountMeta is the content of account.yaml for a Discord account.
type discordAccountMeta struct {
	Name      string    `yaml:"name"      json:"name"`
	PairedAt  time.Time `yaml:"paired_at" json:"paired_at"`
	BotUserID string    `yaml:"bot_user_id,omitempty" json:"bot_user_id,omitempty"`
}

// WriteDiscordAccount creates or overwrites the account directory for a
// Discord bot account. token.txt is written with mode 0600; the directory
// itself is 0700.
func WriteDiscordAccount(name, token string) error {
	dir, err := platformAccountDir("discord", name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create account dir: %w", err)
	}

	meta := discordAccountMeta{
		Name:     name,
		PairedAt: time.Now().UTC(),
	}
	metaData, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	metaPath := filepath.Join(dir, "account.yaml")
	if err := os.WriteFile(metaPath, metaData, 0o600); err != nil {
		return fmt.Errorf("write account.yaml: %w", err)
	}

	tokenPath := filepath.Join(dir, "token.txt")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return fmt.Errorf("write token.txt: %w", err)
	}
	return nil
}

// RemoveDiscordAccount deletes the account directory. Returns (false, nil) if
// not found.
func RemoveDiscordAccount(name string) (bool, error) {
	dir, err := platformAccountDir("discord", name)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return false, nil
	}
	return true, os.RemoveAll(dir)
}

// LoadDiscordToken reads the bot token from token.txt.
func LoadDiscordToken(name string) (string, error) {
	dir, err := platformAccountDir("discord", name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, "token.txt"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("discord account %q not found", name)
		}
		return "", fmt.Errorf("read token: %w", err)
	}
	return string(data), nil
}

// LoadDiscordAccountMeta reads account.yaml for the named account.
func LoadDiscordAccountMeta(name string) (discordAccountMeta, error) {
	dir, err := platformAccountDir("discord", name)
	if err != nil {
		return discordAccountMeta{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "account.yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return discordAccountMeta{}, fmt.Errorf("discord account %q not found", name)
		}
		return discordAccountMeta{}, err
	}
	var meta discordAccountMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return discordAccountMeta{}, fmt.Errorf("parse account.yaml: %w", err)
	}
	return meta, nil
}

// UpdateDiscordAccountMeta writes back account.yaml (used by gateway to
// persist BotUserID after first connect).
func UpdateDiscordAccountMeta(name string, meta discordAccountMeta) error {
	dir, err := platformAccountDir("discord", name)
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

// ListDiscordAccounts returns all paired Discord account names.
func ListDiscordAccounts() ([]string, error) {
	dir, err := meshDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(dir, "discord-accounts")
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
		// Confirm token.txt is present — guards against half-written dirs.
		if _, err := os.Stat(filepath.Join(root, e.Name(), "token.txt")); err == nil {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// DiscordGatewaySockPath returns the path of the gateway Unix socket for an
// account. The socket itself is created by the gateway process.
func DiscordGatewaySockPath(accountName string) (string, error) {
	dir, err := platformAccountDir("discord", accountName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gateway.sock"), nil
}

// gatewayStatusPath returns the path of the gateway's status.json file.
func gatewayStatusPath(platform, accountName string) (string, error) {
	dir, err := platformAccountDir(platform, accountName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "status.json"), nil
}

// GatewayStatus is the shape written atomically by the gateway to status.json.
type GatewayStatus struct {
	Status        string              `json:"status"` // "connecting" | "connected" | "error: …"
	BotUserID     string              `json:"botUserId,omitempty"`
	BotUsername   string              `json:"botUsername,omitempty"`
	MessagesIn    int                 `json:"messagesIn"`
	MessagesOut   int                 `json:"messagesOut"`
	Subscriptions []GatewaySubStatus  `json:"subscriptions,omitempty"`
	DroppedBy     map[string]int      `json:"droppedBy,omitempty"`
}

// GatewaySubStatus describes one active shim subscription on the gateway.
type GatewaySubStatus struct {
	Agent            string `json:"agent"`
	Index            int    `json:"index"`
	RoutingKey       string `json:"routingKey"`
	SubscriptionMode string `json:"subscriptionMode,omitempty"` // "channel" or "guild"
	OverrideCount    int    `json:"overrideCount,omitempty"`    // guild mode: number of per-channel overrides
}

// WriteGatewayStatus atomically writes the gateway's status.json.
func WriteGatewayStatus(platform, accountName string, st GatewayStatus) error {
	path, err := gatewayStatusPath(platform, accountName)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadGatewayStatus reads the gateway's status.json. Returns a zeroed struct
// if the file doesn't exist.
func ReadGatewayStatus(platform, accountName string) (GatewayStatus, error) {
	path, err := gatewayStatusPath(platform, accountName)
	if err != nil {
		return GatewayStatus{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return GatewayStatus{Status: "unknown"}, nil
		}
		return GatewayStatus{}, err
	}
	var st GatewayStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return GatewayStatus{}, err
	}
	return st, nil
}

// agentReferencesDiscordAccount returns true if any registered agent's channel
// list references the named Discord account.
func agentReferencesDiscordAccount(accountName string) (bool, string, error) {
	agents, err := ListAgents()
	if err != nil {
		return false, "", err
	}
	for _, a := range agents {
		for _, ch := range a.Channels {
			if ch.Type != "discord" {
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
