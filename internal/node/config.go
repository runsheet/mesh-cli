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
	// Capacity is how many items the node runs at once: a plain number
	// (fixed) or {min, max}, between which the node scales by the
	// headroom left in its plan's windows (adr-004).
	Capacity Capacity `yaml:"capacity"`
	// Models is the model each kind of session runs on — "" for the
	// CLI's default. A flag on `node start` wins over the file; the
	// platform's agent definitions will win over both once they land
	// (the runsheet goal `agents-on-the-platform`), and these become
	// the fallback for a node that cannot reach them.
	Models Models `yaml:"models,omitempty"`
	// Limits are the thresholds the node paces itself by against its
	// Claude plan (LimitsConfig): the defaults when left out, read again
	// every tick like the capacity.
	Limits LimitsConfig `yaml:"limits,omitempty"`
	// Accounts are the Claude accounts the node's sessions run on
	// (AccountConfig, adr-005) until the platform hands a session its
	// credential at claim. None is the one implicit account `default`:
	// the operator's own CLI login, as before.
	Accounts []AccountConfig `yaml:"accounts,omitempty"`
	// Latchkey is the issuer `mesh node login` signs in to and
	// LatchkeyClient the public client it starts the device flow from
	// (latchkey adr-machine-identity); both are remembered at login so a
	// later one needs no flags.
	Latchkey       string `yaml:"latchkey,omitempty"`
	LatchkeyClient string `yaml:"latchkey_client,omitempty"`
}

// Capacity is node.yaml's `capacity`: a plain N is Min = Max = N, the
// fixed capacity every node had before adr-004; `{min, max}` is a range
// the node scales within by the headroom of its plan's windows.
type Capacity struct {
	Min int `yaml:"min"`
	Max int `yaml:"max"`
}

// Fixed is a capacity that does not scale.
func Fixed(n int) Capacity { return Capacity{Min: n, Max: n} }

// IsRange says the capacity scales: min and max differ.
func (c Capacity) IsRange() bool { return c.Min != c.Max }

// String is how the capacity is logged: "3", or "1–4".
func (c Capacity) String() string {
	if c.IsRange() {
		return fmt.Sprintf("%d–%d", c.Min, c.Max)
	}
	return fmt.Sprint(c.Max)
}

// Validate refuses a range the scaler cannot work within.
func (c Capacity) Validate() error {
	if c.Min < 1 {
		return fmt.Errorf("capacity min %d: must be at least 1", c.Min)
	}
	if c.Max < c.Min {
		return fmt.Errorf("capacity max %d is below its min %d", c.Max, c.Min)
	}
	return nil
}

// UnmarshalYAML takes a plain number or a {min, max} mapping. A plain
// number is read as it always was (LoadConfig makes 0 or less 1); a
// mapping is validated here.
func (c *Capacity) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var v int
		if err := n.Decode(&v); err != nil {
			return fmt.Errorf("capacity: %w", err)
		}
		*c = Fixed(v)
		return nil
	}
	var r struct {
		Min *int `yaml:"min"`
		Max *int `yaml:"max"`
	}
	if err := n.Decode(&r); err != nil {
		return fmt.Errorf("capacity: want a number or {min, max}: %w", err)
	}
	if r.Min == nil || r.Max == nil {
		return errors.New("capacity: a range needs both min and max")
	}
	v := Capacity{Min: *r.Min, Max: *r.Max}
	if err := v.Validate(); err != nil {
		return err
	}
	*c = v
	return nil
}

// MarshalYAML writes a fixed capacity as the plain number it was read
// from, so a file that never used a range saves back unchanged.
func (c Capacity) MarshalYAML() (any, error) {
	if !c.IsRange() {
		return c.Max, nil
	}
	return struct {
		Min int `yaml:"min"`
		Max int `yaml:"max"`
	}{c.Min, c.Max}, nil
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
	Research string `yaml:"research,omitempty"`
	Rebase   string `yaml:"rebase,omitempty"`
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
	case "research":
		v = m.Research
	case "rebase":
		v = m.Rebase
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

// Reloaded is what Loop.Reload reads: the capacity (0 keeps the loop's),
// the limits thresholds (nil keeps the loop's) and the orgs served (nil
// keeps the loop's).
type Reloaded struct {
	Capacity int
	Limits   *LimitsConfig
	Scopes   Scopes
}

// Reloader is Loop.Reload for a node started on cfg and scopes: every
// tick it reads node.yaml again through load (LoadConfig when nil) for
// the capacity, the limits thresholds and the orgs restriction, and asks MyOrgs for the orgs
// the owner belongs to. An org seen for the first time is registered in
// through register (a Node there, as start does) before it is served —
// one that fails to register is tried again next tick; an org no longer
// listed, or no longer in the restriction, is dropped. A node.yaml that
// cannot be read keeps the last good capacity and restriction; a MyOrgs
// that fails, or leaves nothing to serve, keeps the orgs as they were.
func Reloader(gw Gateway, cfg *Config, scopes Scopes, load func() (*Config, error), register func(context.Context, Scope) error) func(context.Context) (Reloaded, error) {
	if load == nil {
		load = LoadConfig
	}
	restrict := cfg.Orgs
	registered := map[string]bool{}
	for _, sc := range scopes {
		registered[sc.NodeID] = true
	}
	return func(ctx context.Context) (Reloaded, error) {
		var errs []error
		var out Reloaded
		if c, err := load(); err != nil {
			errs = append(errs, fmt.Errorf("node.yaml (keeping the last good capacity and limits): %w", err))
		} else {
			limits := c.Limits
			out.Capacity, out.Limits, restrict = c.Capacity.Max, &limits, c.Orgs
		}
		orgs, err := gw.MyOrgs(ctx)
		if err != nil {
			return out, errors.Join(append(errs, fmt.Errorf("my orgs: %w", err))...)
		}
		want := map[string]bool{}
		for _, o := range restrict {
			want[strings.ToLower(strings.TrimSpace(o))] = true
		}
		var served Scopes
		for _, o := range orgs {
			if len(want) > 0 && !want[strings.ToLower(o.Login)] {
				continue
			}
			id, err := cfg.NodeIDIn(o.ID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			sc := Scope{Login: o.Login, ID: o.ID, AccountID: o.AccountID, NodeID: id}
			if !registered[id] && register != nil {
				if err := register(ctx, sc); err != nil {
					errs = append(errs, fmt.Errorf("register in %s: %w", o.Login, err))
					continue
				}
			}
			registered[id] = true
			served = append(served, sc)
		}
		if len(served) == 0 {
			return out, errors.Join(append(errs, errors.New("no org left to serve (keeping the orgs as they were)"))...)
		}
		out.Scopes = served
		return out, errors.Join(errs...)
	}
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
	if c.Capacity.Max <= 0 {
		c.Capacity = Fixed(1)
	}
	if c.Slug == "" {
		if c.Name == "" && c.NodeID == "" && c.OrgLogin == "" && len(c.Orgs) == 0 {
			// only the settings a `mesh node login` wrote, before the
			// machine was ever registered
			return nil, errors.New("this machine is not registered: run `mesh node register --name <name>`")
		}
		return nil, fmt.Errorf("%s is incomplete: re-run `mesh node register`", path)
	}
	if len(c.Orgs) == 0 && c.OrgLogin != "" {
		c.Orgs = []string{c.OrgLogin} // an older, single-org registration
	}
	if err := ValidateAccounts(c.Accounts); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Save writes ~/.mesh/node.yaml.
// LoadSettings reads ~/.mesh/node.yaml as it stands, without insisting
// on a registration: the Latchkey issuer and client `mesh node login`
// records live in the same file and are wanted before `mesh node
// register` has run. A missing file is an empty config.
func LoadSettings() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// SaveLatchkey records the issuer and the public client the device flow
// starts from, keeping whatever registration node.yaml already holds —
// and writing the file when there is none yet, so the first `mesh node
// login` on a machine is the last that needs --client-id.
func SaveLatchkey(issuer, clientID string) error {
	c, err := LoadSettings()
	if err != nil {
		return err
	}
	if c.Latchkey == issuer && c.LatchkeyClient == clientID {
		return nil
	}
	c.Latchkey, c.LatchkeyClient = issuer, clientID
	return c.Save()
}

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
