// Package node is the Mesh node: the loop of mesh-specs trd §3 — heartbeat,
// list the org's ready items, claim up to capacity, run each in a fresh
// session, report progress, finish with a pushed branch — over the
// platform's gateway as the operator whose token it holds.
package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Config is ~/.mesh/node.yaml, written by `mesh node register`.
type Config struct {
	Gateway   string `yaml:"gateway,omitempty"`
	OrgLogin  string `yaml:"org"`
	OrgID     string `yaml:"org_id"`
	AccountID string `yaml:"account_id"`
	OwnerID   string `yaml:"owner_id,omitempty"`
	Slug      string `yaml:"slug"`
	Name      string `yaml:"name"`
	NodeID    string `yaml:"node_id"`
	Capacity  int    `yaml:"capacity"`
}

// ConfigPath is ~/.mesh/node.yaml.
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "node.yaml"), nil
}

// LoadConfig reads ~/.mesh/node.yaml.
func LoadConfig() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("this machine is not registered: run `mesh node register --org <login> --name <name>`")
		}
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Gateway == "" {
		c.Gateway = gateway.DefaultURL
	}
	if c.Capacity <= 0 {
		c.Capacity = 1
	}
	if c.OrgID == "" || c.NodeID == "" || c.Slug == "" {
		return nil, fmt.Errorf("%s is incomplete: re-run `mesh node register`", path)
	}
	return &c, nil
}

// Save writes ~/.mesh/node.yaml.
func (c *Config) Save() error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// WorkRoot is ~/.mesh/work, the parent of every workspace.
func WorkRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "work"), nil
}

// Slugify turns a name into a slug: lowercase, [a-z0-9-], no doubles.
func Slugify(name string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
