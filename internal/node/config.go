// Package node is the Mesh node: the loop of mesh-specs trd §3 — heartbeat,
// list the org's ready items, claim up to capacity, run each in a fresh
// session, report progress, finish with a pushed branch — over the
// platform's gateway as the operator whose token it holds.
package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Config is ~/.mesh/node.yaml, written by `mesh node register`. A node
// serves every org its owner belongs to (a Node per org on the platform,
// id from (org, slug)), or only those in Orgs when set. The single-org
// fields are what older registrations wrote; read as Orgs of one.
type Config struct {
	Gateway string `yaml:"gateway,omitempty"`
	// Orgs restricts the node to these logins; empty is every org of the owner.
	Orgs      []string `yaml:"orgs,omitempty"`
	OrgLogin  string   `yaml:"org,omitempty"`
	OrgID     string   `yaml:"org_id,omitempty"`
	AccountID string   `yaml:"account_id,omitempty"`
	NodeID    string   `yaml:"node_id,omitempty"`
	OwnerID   string   `yaml:"owner_id,omitempty"`
	Slug      string   `yaml:"slug"`
	Name      string   `yaml:"name"`
	Capacity  int      `yaml:"capacity"`
	// Models is the model each kind of session runs on — "" for the
	// CLI's default. A flag on `node start` wins over the file; the
	// platform's agent definitions will win over both once they land
	// (the runsheet goal `agents-on-the-platform`), and these become
	// the fallback for a node that cannot reach them.
	Models Models `yaml:"models,omitempty"`
}

// Models names a model per kind of session: Default when a kind has
// none. Values are what the CLI takes — an alias ("opus", "sonnet",
// "haiku") or a full id ("claude-opus-5").
type Models struct {
	Default  string `yaml:"default,omitempty"`
	Build    string `yaml:"build,omitempty"`
	Evaluate string `yaml:"evaluate,omitempty"`
	Plan     string `yaml:"plan,omitempty"`
	Channel  string `yaml:"channel,omitempty"`
}

// For is the model of one kind of session, falling back to Default.
func (m Models) For(kind string) string {
	var v string
	switch kind {
	case "build":
		v = m.Build
	case "evaluate":
		v = m.Evaluate
	case "plan":
		v = m.Plan
	case "channel":
		v = m.Channel
	}
	if v == "" {
		v = m.Default
	}
	return v
}

// Scope is one org the node serves, with the node's id there.
type Scope struct {
	Login     string
	ID        string
	AccountID string
	NodeID    string
}

// Scopes are the orgs a node serves.
type Scopes []Scope

// ByOrg finds the scope of an org.
func (s Scopes) ByOrg(orgID string) (Scope, bool) {
	for _, sc := range s {
		if sc.ID == orgID {
			return sc, true
		}
	}
	return Scope{}, false
}

// LoginOf is an org's login, or "" when the node does not serve it.
func (s Scopes) LoginOf(orgID string) string {
	sc, _ := s.ByOrg(orgID)
	return sc.Login
}

// Logins lists the orgs served, in order.
func (s Scopes) Logins() []string {
	out := make([]string, 0, len(s))
	for _, sc := range s {
		out = append(out, sc.Login)
	}
	return out
}

// NodeIDIn is the node's id in an org: from (org, slug), so it converges.
func (c *Config) NodeIDIn(orgID string) (string, error) {
	id, err := uuid.Parse(orgID)
	if err != nil {
		return "", fmt.Errorf("org id %q: %w", orgID, err)
	}
	return gateway.NodeID(id, c.Slug).String(), nil
}

// ResolveScopes lists the orgs the owner belongs to on the platform,
// keeps those the config names (all, when it names none), and gives the
// node its id in each. Orgs the config names that the owner is not in
// are an error: the node would never see their items.
func ResolveScopes(ctx context.Context, gw Gateway, c *Config) (Scopes, error) {
	orgs, err := gw.MyOrgs(ctx)
	if err != nil {
		return nil, fmt.Errorf("my orgs: %w", err)
	}
	want := map[string]bool{}
	for _, o := range c.Orgs {
		want[strings.ToLower(strings.TrimSpace(o))] = true
	}
	var scopes Scopes
	for _, o := range orgs {
		if len(want) > 0 && !want[strings.ToLower(o.Login)] {
			continue
		}
		id, err := c.NodeIDIn(o.ID)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, Scope{Login: o.Login, ID: o.ID, AccountID: o.AccountID, NodeID: id})
		delete(want, strings.ToLower(o.Login))
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for o := range want {
			missing = append(missing, o)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("not a member of %s (the node.yaml orgs list names them)", strings.Join(missing, ", "))
	}
	if len(scopes) == 0 {
		return nil, errors.New("the token's person belongs to no claimed org")
	}
	return scopes, nil
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
			return nil, errors.New("this machine is not registered: run `mesh node register --name <name>`")
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
	if c.Slug == "" {
		return nil, fmt.Errorf("%s is incomplete: re-run `mesh node register`", path)
	}
	if len(c.Orgs) == 0 && c.OrgLogin != "" {
		c.Orgs = []string{c.OrgLogin} // an older, single-org registration
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
