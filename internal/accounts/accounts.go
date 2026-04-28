package accounts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"projectmesh.io/mesh-cli/internal/paths"
)

// GatewayStatus is the shape written atomically by the gateway to status.json.
type GatewayStatus struct {
	Status        string             `json:"status"` // "connecting" | "connected" | "error: …"
	BotUserID     string             `json:"botUserId,omitempty"`
	BotUsername   string             `json:"botUsername,omitempty"`
	MessagesIn    int                `json:"messagesIn"`
	MessagesOut   int                `json:"messagesOut"`
	Subscriptions []GatewaySubStatus `json:"subscriptions,omitempty"`
	DroppedBy     map[string]int     `json:"droppedBy,omitempty"`
}

// GatewaySubStatus describes one active shim subscription on the gateway.
type GatewaySubStatus struct {
	Agent            string   `json:"agent"`
	Index            int      `json:"index"`
	RoutingKey       string   `json:"routingKey"`
	SubscriptionMode string   `json:"subscriptionMode,omitempty"` // "channel" or "guild"
	OverrideCount    int      `json:"overrideCount,omitempty"`    // guild mode: number of per-channel overrides
	Overrides        []string `json:"overrides,omitempty"`        // guild mode: channelIds explicitly overridden
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

func gatewayStatusPath(platform, accountName string) (string, error) {
	dir, err := paths.PlatformAccountDir(platform, accountName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "status.json"), nil
}

// WriteDiscordAccount creates or overwrites the account directory for a
// Discord bot account. token.txt is written with mode 0600; the directory
// itself is 0700.
func WriteDiscordAccount(name, token string) error {
	dir, err := paths.PlatformAccountDir("discord", name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create account dir: %w", err)
	}

	meta := DiscordAccountMeta{
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
	dir, err := paths.PlatformAccountDir("discord", name)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return false, nil
	}
	return true, os.RemoveAll(dir)
}

// DiscordAccountMeta is the content of account.yaml for a Discord account.
type DiscordAccountMeta struct {
	Name      string    `yaml:"name"      json:"name"`
	PairedAt  time.Time `yaml:"paired_at" json:"paired_at"`
	BotUserID string    `yaml:"bot_user_id,omitempty" json:"bot_user_id,omitempty"`
}

// LoadDiscordToken reads the bot token from token.txt.
func LoadDiscordToken(name string) (string, error) {
	dir, err := paths.PlatformAccountDir("discord", name)
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
func LoadDiscordAccountMeta(name string) (DiscordAccountMeta, error) {
	dir, err := paths.PlatformAccountDir("discord", name)
	if err != nil {
		return DiscordAccountMeta{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "account.yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return DiscordAccountMeta{}, fmt.Errorf("discord account %q not found", name)
		}
		return DiscordAccountMeta{}, err
	}
	var meta DiscordAccountMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return DiscordAccountMeta{}, fmt.Errorf("parse account.yaml: %w", err)
	}
	return meta, nil
}

// UpdateDiscordAccountMeta writes back account.yaml (used by gateway to
// persist BotUserID after first connect).
func UpdateDiscordAccountMeta(name string, meta DiscordAccountMeta) error {
	dir, err := paths.PlatformAccountDir("discord", name)
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
	dir, err := paths.MeshDir()
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
		if _, err := os.Stat(filepath.Join(root, e.Name(), "token.txt")); err == nil {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// WriteWhatsappAccount creates the account directory and writes account.yaml.
// It does NOT write store.db — that is done by the whatsmeow pairing handshake.
// The directory is created with mode 0700; account.yaml is mode 0600.
func WriteWhatsappAccount(name string, meta WhatsappAccountMeta) error {
	dir, err := paths.PlatformAccountDir("whatsapp", name)
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

// RemoveWhatsappAccount removes the account directory. Returns (false, nil) if
// not found.
func RemoveWhatsappAccount(name string) (bool, error) {
	dir, err := paths.PlatformAccountDir("whatsapp", name)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return false, nil
	}
	return true, os.RemoveAll(dir)
}

// WhatsappAccountMeta is the content of account.yaml for a WhatsApp account.
type WhatsappAccountMeta struct {
	Name        string    `json:"name"`
	Mode        string    `json:"mode"`       // always "web" in slice 2
	PairedAt    time.Time `json:"paired_at"`
	PhoneNumber string    `json:"phone_number,omitempty"`
}

// LoadWhatsappAccountMeta reads account.yaml for the named WhatsApp account.
func LoadWhatsappAccountMeta(name string) (WhatsappAccountMeta, error) {
	dir, err := paths.PlatformAccountDir("whatsapp", name)
	if err != nil {
		return WhatsappAccountMeta{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "account.yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return WhatsappAccountMeta{}, fmt.Errorf("whatsapp account %q not found", name)
		}
		return WhatsappAccountMeta{}, err
	}
	var meta WhatsappAccountMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return WhatsappAccountMeta{}, fmt.Errorf("parse whatsapp account.yaml: %w", err)
	}
	return meta, nil
}

// UpdateWhatsappAccountMeta writes back account.yaml (e.g. to persist
// PhoneNumber after pairing completes).
func UpdateWhatsappAccountMeta(name string, meta WhatsappAccountMeta) error {
	dir, err := paths.PlatformAccountDir("whatsapp", name)
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
	dir, err := paths.MeshDir()
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
		if _, err := os.Stat(filepath.Join(root, e.Name(), "account.yaml")); err == nil {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// WhatsappStorePath returns the store.db path for a WhatsApp account.
func WhatsappStorePath(name string) (string, error) {
	dir, err := paths.PlatformAccountDir("whatsapp", name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "store.db"), nil
}
