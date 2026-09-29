// Package node is the Mesh node: the loop of mesh-specs trd §3 — heartbeat,
// list the org's ready items, claim up to capacity, run each in a fresh
// session, report progress, finish with a pushed branch — over the
// platform's gateway as the operator whose token it holds.
package node

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/selfupdate"
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
	// ProjectBudgets is each project's daily budget in list-price
	// dollars, shared by the api accounts naming that project
	// (adr-005-capacity-across-accounts); a project not listed has none.
	ProjectBudgets map[string]float64 `yaml:"project_budgets,omitempty"`
	// Budgets caps one session's cost by kind (build, evaluate, plan,
	// rebase, research) and then by the model tier it runs on (haiku,
	// sonnet, opus — a claude-* id resolves to its tier via
	// Config.KindModelBudget/ModelTier), read live like Limits (ADR
	// 012). A kind or tier left out takes its default: build haiku 2,
	// sonnet 4, opus 10; the other kinds keep their historical flat
	// fallback. The platform is meant to override this per claimed item
	// or project once schema.json carries a `budgets` field; it does
	// not yet.
	Budgets map[string]map[string]float64 `yaml:"budgets,omitempty"`
	// Codex is OpenAI's Codex CLI on this node (CodexConfig): its
	// binary, its accounts and the Codex model for each Claude tier.
	// Nothing runs on it yet.
	Codex CodexConfig `yaml:"codex,omitempty"`
	// Sandbox is whether and how sessions run in their own pods
	// (SandboxConfig, ADR 007).
	Sandbox SandboxConfig `yaml:"sandbox,omitempty"`
	// ToolResults caps what one tool result can put into a session's
	// context (ToolResults, trd §3 Tool results).
	ToolResults ToolResults `yaml:"tool_results,omitempty"`
	// Latchkey is the issuer `mesh node login` signs in to and
	// LatchkeyClient the public client it starts the device flow from
	// (latchkey adr-machine-identity); both are remembered at login so a
	// later one needs no flags.
	Latchkey       string `yaml:"latchkey,omitempty"`
	LatchkeyClient string `yaml:"latchkey_client,omitempty"`
	// Update is node.yaml's `update:` block (UpdateConfig): how long a
	// new process keeps trying to confirm an update before it gives up.
	Update UpdateConfig `yaml:"update,omitempty"`
	// Context is the size budget for the project's context on disk
	// (ContextConfig), read again every tick like the accounts.
	Context ContextConfig `yaml:"context,omitempty"`
}

// The context budget's defaults, in bytes: a design document past
// DefaultDocBytesMax, or past what is left of DefaultContextBytesMax,
// is listed in the index and fetched on demand instead.
const (
	DefaultContextBytesMax = 240 * 1024
	DefaultDocBytesMax     = 60 * 1024
)

// ContextConfig is node.yaml's `context:` block, in bytes: the total a
// session's context may put on disk and the most one design document
// may. Zero is the default.
type ContextConfig struct {
	BytesMax    int `yaml:"bytes_max,omitempty"`
	DocBytesMax int `yaml:"doc_bytes_max,omitempty"`
}

// Effective is c with each zero field at its default.
func (c ContextConfig) Effective() ContextConfig {
	if c.BytesMax == 0 {
		c.BytesMax = DefaultContextBytesMax
	}
	if c.DocBytesMax == 0 {
		c.DocBytesMax = DefaultDocBytesMax
	}
	return c
}

// Validate refuses a negative size, naming the key.
func (c ContextConfig) Validate() error {
	if c.BytesMax < 0 {
		return errors.New("context.bytes_max: a size cannot be negative")
	}
	if c.DocBytesMax < 0 {
		return errors.New("context.doc_bytes_max: a size cannot be negative")
	}
	return nil
}

// UpdateConfig is node.yaml's `update:` block.
type UpdateConfig struct {
	// ConfirmWithin is how long a new process keeps retrying its
	// heartbeat to confirm a pending update before it gives up on
	// confirming; empty is Config.ConfirmWithin's default of 5 minutes.
	// A platform blip or a busy box within the window is not a failed
	// start.
	ConfirmWithin time.Duration `yaml:"confirm_within,omitempty"`
}

// ConfirmWithin is the confirm window in force: node.yaml's
// update.confirm_within, or 5 minutes when it names none.
func (c *Config) ConfirmWithin() time.Duration {
	if c.Update.ConfirmWithin > 0 {
		return c.Update.ConfirmWithin
	}
	return selfupdate.DefaultConfirmWithin
}

// DefaultToolResultsKB is the tool result cap when node.yaml names none.
const DefaultToolResultsKB = 16

// ToolResults is node.yaml's `tool_results`: MaxKB is the most a tool
// result may put into a session's context, in KB — left out,
// DefaultToolResultsKB; 0 turns the cap off. A build session's Bash
// output over it is trimmed to head and tail by its PreToolUse hook
// (`mesh tool-trim`), and the CLI's own MCP and Bash output limits are
// set from it (ToolResultsEnv).
type ToolResults struct {
	MaxKB *int `yaml:"max_kb,omitempty"`
}

// KB is the cap in force: MaxKB, else the default; never negative.
func (t ToolResults) KB() int {
	if t.MaxKB == nil {
		return DefaultToolResultsKB
	}
	return max(*t.MaxKB, 0)
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

// PlanModel is the planner's model: Plan when it is set explicitly,
// otherwise the strongest of the other kinds configured, otherwise opus.
// The planner chooses every item's model, so it runs on the strongest
// there is.
func (m Models) PlanModel() string {
	if m.Plan != "" {
		return m.Plan
	}
	if s := Strongest(m.Default, m.Build, m.Evaluate, m.Channel, m.Research); s != "" {
		return s
	}
	return "opus"
}

// CodexConfig is node.yaml's `codex:` block.
type CodexConfig struct {
	// Binary is the codex executable; empty is "codex" on PATH.
	Binary string `yaml:"binary,omitempty"`
	// Accounts are the OpenAI accounts Codex sessions run on.
	Accounts []CodexAccountConfig `yaml:"accounts,omitempty"`
	// Models maps the Claude tiers to Codex models (CodexModel).
	Models CodexModels `yaml:"models,omitempty"`
}

// CodexAccountConfig is one Codex account: a ChatGPT-plan login in its
// own Home (`CODEX_HOME=<home> codex login`), or an API key in the
// variable APIKeyEnv names or the file APIKeyFile names. Exactly one.
type CodexAccountConfig struct {
	Name       string `yaml:"name"`
	Home       string `yaml:"home,omitempty"`
	APIKeyEnv  string `yaml:"api_key_env,omitempty"`
	APIKeyFile string `yaml:"api_key_file,omitempty"`
}

// Harness is the account as the Codex adapter takes it.
func (a CodexAccountConfig) Harness() harness.CodexAccount {
	return harness.CodexAccount{Name: a.Name, Home: a.Home, APIKeyEnv: a.APIKeyEnv, APIKeyFile: a.APIKeyFile}
}

// CodexModels is the Codex model for each Claude tier.
type CodexModels struct {
	Opus   string `yaml:"opus,omitempty"`
	Sonnet string `yaml:"sonnet,omitempty"`
	Haiku  string `yaml:"haiku,omitempty"`
}

// IsZero leaves an empty block out of a saved node.yaml.
func (c CodexConfig) IsZero() bool {
	return c.Binary == "" && len(c.Accounts) == 0 && c.Models == CodexModels{}
}

// Validate refuses accounts Codex could not run on: a name missing or
// used twice, or not exactly one of home, api_key_env and api_key_file.
func (c CodexConfig) Validate() error {
	seen := map[string]bool{}
	for i, a := range c.Accounts {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			return fmt.Errorf("codex.accounts[%d]: a name is required", i)
		}
		if seen[name] {
			return fmt.Errorf("codex.accounts[%d]: name %q is used twice", i, name)
		}
		seen[name] = true
		n := 0
		for _, v := range []string{a.Home, a.APIKeyEnv, a.APIKeyFile} {
			if strings.TrimSpace(v) != "" {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("codex account %q: want exactly one of home, api_key_env and api_key_file", name)
		}
	}
	return nil
}

// The Codex model each Claude tier maps to when node.yaml's codex:
// models names none (https://learn.chatgpt.com/docs/models). gpt-6-astra
// costs $1.29 for one evaluation where gpt-6-sol costs about $0.45, so
// astra is used only when an item pins it (EvaluatorPin), never a default.
const (
	DefaultCodexOpus   = "gpt-6-sol"
	DefaultCodexSonnet = "gpt-6-luna"
	DefaultCodexHaiku  = "gpt-6-luna"
)

// CodexModel is the Codex model for a Claude model: an alias or a full
// id naming opus, sonnet or haiku (by ModelRank) takes that tier's
// model from Models, else the tier's default (gpt-6-sol, gpt-6-luna,
// gpt-6-luna). Empty, or a model of no tier, is "": Codex's own
// default.
func (c CodexConfig) CodexModel(claudeModel string) string {
	switch ModelRank(claudeModel) {
	case 3:
		return cmp.Or(c.Models.Opus, DefaultCodexOpus)
	case 2:
		return cmp.Or(c.Models.Sonnet, DefaultCodexSonnet)
	case 1:
		return cmp.Or(c.Models.Haiku, DefaultCodexHaiku)
	}
	return ""
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
	// Capacity is node.yaml's, fixed or a range; zero keeps the loop's.
	Capacity Capacity
	Limits   *LimitsConfig
	Scopes   Scopes
	// Spending is node.yaml's accounts and project budgets; nil keeps
	// the loop's.
	Spending *Spending
	// Context is node.yaml's `context:` budget; nil keeps the loop's.
	Context *ContextConfig
}

// Spending is what the node may spend on: node.yaml's accounts,
// project budgets and per-kind, per-model session budgets, read again
// every tick like the capacity.
type Spending struct {
	Accounts       []AccountConfig
	ProjectBudgets map[string]float64
	// Budgets is node.yaml's `budgets:` (ADR 012).
	Budgets map[string]map[string]float64
}

// Reloader is Loop.Reload for a node started on cfg and scopes: every
// tick it reads node.yaml again through load (LoadConfig when nil) for
// the capacity, the limits thresholds, the accounts and project budgets,
// the context budget and the orgs restriction, and asks MyOrgs for the orgs
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
			out.Capacity, out.Limits, restrict = c.Capacity, &limits, c.Orgs
			out.Spending = &Spending{Accounts: c.Accounts, ProjectBudgets: c.ProjectBudgets, Budgets: c.Budgets}
			budget := c.Context
			out.Context = &budget
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
	for p, b := range c.ProjectBudgets {
		if b < 0 {
			return nil, fmt.Errorf("%s: project_budgets[%s]: a budget cannot be negative", path, p)
		}
	}
	if err := c.Codex.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Context.Validate(); err != nil {
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
