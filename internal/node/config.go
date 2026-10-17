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
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/selfupdate"
)

// Config is ~/.mesh/node.yaml, written by `mesh node register`. A node
// serves every org its owner belongs to (a Node per org on the platform,
// id from (org, slug)), or only those in Orgs when set. The single-org
// fields are what older registrations wrote; read as Orgs of one.
// `host:` is `{load_per_core_max: 1.5, package_temp_max_c: 90,
// cpu_some_avg60_max: 25, quiet_fraction: 0.8}` by default; a threshold
// left out, or set to 0, takes its default.
// `takes:` names the kinds of session (build, evaluate, rebase, plan,
// research) this node claims, as a list or a per-kind cap map; left
// out, every kind, as before (goal a-node-says-which-sessions-it-takes).
type Config struct {
	Gateway string `yaml:"gateway,omitempty"`
	// Orgs restricts the node to these logins; empty is every org of the owner.
	Orgs []string `yaml:"orgs,omitempty"`
	// OrgLogin is `org:`. With owner: org it is the org that owns this
	// node, by slug or id, required — LoadConfig refuses one without
	// the other — and resolved to an id once at registration, cached
	// as OrgID (goal node-yaml-says-owner-org). Without it, it is only
	// an older single-org registration's login, written beside org_id
	// and account_id and read as Orgs of one; `org:` with neither
	// owner: org nor account_id is refused.
	OrgLogin string `yaml:"org,omitempty"`
	// OrgID is `org_id:`. With owner: org it is OrgLogin resolved to an
	// id from the machine token's orgs read at the first owner
	// registration — never signIn — cached so a later start registers
	// without the read, as EnterpriseID is, and sent as the
	// registration's ownerId. Without it, an older single-org
	// registration's org id.
	OrgID     string `yaml:"org_id,omitempty"`
	AccountID string `yaml:"account_id,omitempty"`
	// NodeID is the node's own single id, from the platform's NodeAnswer
	// (goal the-node-serves-its-owners-orgs, item 12). On the legacy
	// per-org path — a platform that answers with neither owner nor
	// scopes — NodeID is unused: each Scope keeps its own per-org id
	// from NodeIDIn instead.
	NodeID string `yaml:"node_id,omitempty"`
	// NodeIDSource is how NodeID was arrived at on the owner contract:
	// "derived" (OwnedNodeID computed it) or "adopted" (a refused
	// registration named it, AdoptedID). Written by SaveRegistration;
	// unchanged by a start that registers under the saved node_id
	// (ADR-015's amendment 2026-10-03).
	NodeIDSource string `yaml:"node_id_source,omitempty"`
	// Contract is the registration the node last made (ADR-015,
	// Registration and the loop; TRD §3 step 1): ContractOwner when the
	// platform answered the owner registration, ContractPerOrg when it
	// ran the per-org path. Saved by every registration beside NodeID;
	// empty, on a node.yaml from before it, means per-org.
	Contract string `yaml:"contract,omitempty"`
	OwnerID  string `yaml:"owner_id,omitempty"`
	Slug     string `yaml:"slug"`
	Name     string `yaml:"name"`
	// Pair are the people paired with this node's build and rebase
	// sessions: each a Co-Authored-By trailer on every commit they make
	// (Pairers). The interim source until the platform serves an item's
	// paired field (ADR-024).
	Pair []Pairer `yaml:"pair,omitempty"`
	// Owner is the node's ownership model: "" or "person" (the
	// default, bring-your-own infra — the node serves every org its
	// token's person belongs to), "enterprise" (shared infra — the
	// node serves every org the enterprise has) or "org" (a company's
	// own box — the node serves that one org and no other). LoadConfig
	// rejects any other value.
	Owner string `yaml:"owner,omitempty"`
	// Enterprise is the enterprise that owns this node, by slug or id,
	// required when Owner is "enterprise" — LoadConfig refuses one
	// without the other. Resolved to an id once at registration,
	// cached (item 3 of this goal); until then it is the slug or id as
	// written here.
	Enterprise string `yaml:"enterprise,omitempty"`
	// EnterpriseID is Enterprise resolved to an id through myEnterprises
	// at the first owner registration, cached so a later start on a
	// token that cannot list enterprises still registers (ADR-015's
	// amendment 2026-10-03). Written by SaveRegistration beside node_id;
	// sent as the registration's ownerId.
	EnterpriseID string `yaml:"enterprise_id,omitempty"`
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
	// Accounts is the retired `accounts:` key, read only to refuse it
	// (Retired): a session runs only on the credentials the platform
	// issues at claim (adr-005's amendments 2026-10). AccountConfig is the
	// shape of those, never of node.yaml.
	Accounts []any `yaml:"accounts,omitempty"`
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
	// Claim is node.yaml's `claim:` block (ClaimConfig), read again
	// every tick like the context budget.
	Claim ClaimConfig `yaml:"claim,omitempty"`
	// Shares is node.yaml's `shares:` block: the weight Floors divides
	// capacity by, keyed by a served org's login (or its slug when it
	// has none, as Orgs names it) — a person's node's own setting for
	// ADR-023's floors, read again every tick like the claim limits; an
	// enterprise's own `shares` (a later item) takes it over when both
	// are present.
	Shares map[string]int `yaml:"shares,omitempty"`
	// Host is node.yaml's `host:` block (HostConfig): the load and
	// package-temperature thresholds past which the machine is judged
	// hot (ADR-004's amendment, the-node-backs-off-a-hot-host) — left
	// out, or a field left zero, takes its default, read again every
	// tick like the claim limits.
	Host HostConfig `yaml:"host,omitempty"`
	// Disk is node.yaml's `disk:` block (DiskConfig): the free space
	// under which the node claims nothing (diskfloor.go) — left out, or
	// a field left zero, takes its default, read again every tick like
	// `host:`.
	Disk DiskConfig `yaml:"disk,omitempty"`
	// Takes is node.yaml's `takes:` block (Takes): which kinds of
	// session this node claims, and each kind's own cap — read again
	// every tick like the claim limits. Left out, every kind (Takes.Takes
	// is true for anything).
	Takes Takes `yaml:"takes,omitempty"`
	// ValidationCacheMaxBytes is the most the node's cache root
	// (SharedCacheRoot: the shared tree cache and the Go build cache
	// beside it) may hold, in bytes; left out or 0,
	// DefaultValidationCacheMaxBytes. Over it SharedCache.Enforce evicts.
	ValidationCacheMaxBytes int64 `yaml:"validation_cache_max_bytes,omitempty"`
}

// DefaultValidationCacheMaxBytes is the cache root's cap when node.yaml
// names none: 20 GiB.
const DefaultValidationCacheMaxBytes int64 = 20 << 30

// ValidationCacheCap is the cache root's cap in force:
// ValidationCacheMaxBytes, else the default.
func (c *Config) ValidationCacheCap() int64 {
	if c == nil || c.ValidationCacheMaxBytes <= 0 {
		return DefaultValidationCacheMaxBytes
	}
	return c.ValidationCacheMaxBytes
}

// ClaimConfig is node.yaml's `claim:` block. UnmergedPerRepo is the most
// pull requests a repo may have open and waiting to merge (items in
// `review`) before the node claims no fresh item of that repo; started
// work there is still claimed (ADR-017). Zero, the default, is no limit.
type ClaimConfig struct {
	UnmergedPerRepo int `yaml:"unmerged_per_repo,omitempty"`
}

// Validate refuses a negative limit, naming the key.
func (c ClaimConfig) Validate() error {
	if c.UnmergedPerRepo < 0 {
		return errors.New("claim.unmerged_per_repo: a limit cannot be negative")
	}
	return nil
}

// KnownKinds are the five kinds a node session can be, in the order
// Takes.Kinds returns when node.yaml names none.
var KnownKinds = []harness.Kind{
	harness.KindBuild, harness.KindEvaluate, harness.KindRebase, harness.KindPlan, harness.KindResearch,
}

// knownKindNames is KnownKinds joined for an error message.
func knownKindNames() string {
	names := make([]string, len(KnownKinds))
	for i, k := range KnownKinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// isKnownKind says whether kind is one of KnownKinds.
func isKnownKind(kind string) bool {
	for _, k := range KnownKinds {
		if string(k) == kind {
			return true
		}
	}
	return false
}

// TakeKind is one entry of Takes: a kind this node claims, and its own
// cap — 0 for none.
type TakeKind struct {
	Kind string
	Cap  int
}

// Takes is node.yaml's `takes:` block: the kinds of session (harness.Kind)
// this node claims, in the order node.yaml named them, each with its own
// cap or none — read from a sequence (`takes: [evaluate]`) or a mapping
// of kind to cap (`takes: {evaluate: 3}`). The zero value (`takes:` left
// out) takes every kind, uncapped: Takes.Takes is true for anything and
// Kinds lists KnownKinds.
type Takes []TakeKind

// Takes says whether this node claims a session of kind: true for every
// kind when node.yaml named none.
func (t Takes) Takes(kind harness.Kind) bool {
	if len(t) == 0 {
		return true
	}
	for _, k := range t {
		if k.Kind == string(kind) {
			return true
		}
	}
	return false
}

// Cap is the cap node.yaml set on kind, and whether it set one — false
// when kind is uncapped (named with no cap, or not named, or named by a
// bare list).
func (t Takes) Cap(kind harness.Kind) (int, bool) {
	for _, k := range t {
		if k.Kind == string(kind) {
			if k.Cap > 0 {
				return k.Cap, true
			}
			return 0, false
		}
	}
	return 0, false
}

// Kinds lists the kinds this node takes, in the order node.yaml named
// them — or KnownKinds, build first, when it names none.
func (t Takes) Kinds() []harness.Kind {
	if len(t) == 0 {
		return KnownKinds
	}
	out := make([]harness.Kind, len(t))
	for i, k := range t {
		out[i] = harness.Kind(k.Kind)
	}
	return out
}

// String is how Takes is logged and shown: "evaluate (at most 3), plan",
// or "every kind" when node.yaml named none.
func (t Takes) String() string {
	if len(t) == 0 {
		return "every kind"
	}
	parts := make([]string, len(t))
	for i, k := range t {
		if k.Cap > 0 {
			parts[i] = fmt.Sprintf("%s (at most %d)", k.Kind, k.Cap)
		} else {
			parts[i] = k.Kind
		}
	}
	return strings.Join(parts, ", ")
}

// Validate refuses an unknown kind, naming it and the five known ones,
// and an empty, non-nil takes: (node.yaml wrote `takes: []`, which would
// claim nothing; leave the key out instead to take every kind). A cap
// under 1 is refused as the mapping shape is parsed (Takes.UnmarshalYAML),
// since only that shape ever gives one.
func (t Takes) Validate() error {
	if t == nil {
		return nil
	}
	if len(t) == 0 {
		return errors.New("takes: names no kind; leave it out to take every kind")
	}
	for _, k := range t {
		if !isKnownKind(k.Kind) {
			return fmt.Errorf("takes: unknown kind %q (one of %s)", k.Kind, knownKindNames())
		}
	}
	return nil
}

// UnmarshalYAML takes a sequence of kind names (`takes: [evaluate]`,
// uncapped) or a mapping of kind to cap (`takes: {evaluate: 3}`),
// keeping the order node.yaml wrote them in so the loop can render it
// back. A mapping's cap under 1 is refused here, since that shape always
// gives one; an unknown kind or an empty takes: is Validate's to refuse,
// once the whole file is read.
func (t *Takes) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		var kinds []string
		if err := n.Decode(&kinds); err != nil {
			return fmt.Errorf("takes: %w", err)
		}
		out := make(Takes, 0, len(kinds))
		for _, k := range kinds {
			out = append(out, TakeKind{Kind: k})
		}
		*t = out
		return nil
	case yaml.MappingNode:
		if len(n.Content)%2 != 0 {
			return errors.New("takes: an invalid mapping")
		}
		out := make(Takes, 0, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			var kind string
			if err := n.Content[i].Decode(&kind); err != nil {
				return fmt.Errorf("takes: %w", err)
			}
			var cap int
			if err := n.Content[i+1].Decode(&cap); err != nil {
				return fmt.Errorf("takes: %w", err)
			}
			if cap < 1 {
				return fmt.Errorf("takes: %s: cap %d must be at least 1", kind, cap)
			}
			out = append(out, TakeKind{Kind: kind, Cap: cap})
		}
		*t = out
		return nil
	default:
		return errors.New("takes: want a list of kinds or a mapping of kind to cap")
	}
}

// MarshalYAML writes Takes back as it was read: a sequence of kind names
// when no entry has a cap, else a mapping of kind to cap in the same
// order — so a node.yaml using either shape saves back unchanged.
func (t Takes) MarshalYAML() (any, error) {
	capped := false
	for _, k := range t {
		if k.Cap > 0 {
			capped = true
			break
		}
	}
	if !capped {
		kinds := make([]string, len(t))
		for i, k := range t {
			kinds[i] = k.Kind
		}
		return kinds, nil
	}
	node := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range t {
		var keyNode, valNode yaml.Node
		if err := keyNode.Encode(k.Kind); err != nil {
			return nil, err
		}
		if err := valNode.Encode(k.Cap); err != nil {
			return nil, err
		}
		node.Content = append(node.Content, &keyNode, &valNode)
	}
	return node, nil
}

// DefaultLoadPerCoreMax, DefaultPackageTempMaxC and DefaultOverloadPerCore
// are host.load_per_core_max's, host.package_temp_max_c's and
// host.overload_per_core's defaults, in force when node.yaml leaves the
// `host:` section, or that field, out or zero.
// host.overload_cpu_some_avg60 has no default: left out or zero, it is
// not judged at all, Overloaded going by load per core alone.
const (
	DefaultLoadPerCoreMax  = 1.5
	DefaultPackageTempMaxC = 90.0
	DefaultOverloadPerCore = 1.0
)

// DefaultCPUSomeAvg60Max is host.cpu_some_avg60_max's default: the node
// user's slice's CPU pressure some avg60, as a percentage, past which
// the host is hot when the slice's pressure is read (ADR-031).
// DefaultQuietFraction is host.quiet_fraction's default: once hot, the
// host is quiet again only below this fraction of the threshold that
// judges it — pressure or load per core (ADR-031, hysteresis).
const (
	DefaultCPUSomeAvg60Max = 25.0
	DefaultQuietFraction   = 0.8
)

// DefaultStarveAfter is host.starve_after's default: a build silent
// this long on an overloaded host is stopped and released, a third of
// the platform's 45-minute stall.
const DefaultStarveAfter = 15 * time.Minute

// HostConfig is node.yaml's `host:` block: the load-per-core and
// package-temperature thresholds past which the scaler judges the
// machine hot and backs capacity off (ADR-004's amendment,
// the-node-backs-off-a-hot-host), and the load-per-core and, optionally,
// CPU-pressure thresholds past which a session is judged starved by an
// overloaded host rather than failed on its own (MachineReading.Overloaded).
// A field left out, or zero, takes its default (Effective) — except
// OverloadCPUSomeAvg60, whose zero disables that half of the check.
type HostConfig struct {
	LoadPerCoreMax  float64 `yaml:"load_per_core_max,omitempty"`
	PackageTempMaxC float64 `yaml:"package_temp_max_c,omitempty"`
	// OverloadPerCore is host.overload_per_core: the load-per-core past
	// which Overloaded is true. Default DefaultOverloadPerCore.
	OverloadPerCore float64 `yaml:"overload_per_core,omitempty"`
	// CPUSomeAvg60Max is host.cpu_some_avg60_max: the node user's
	// slice's cpu.pressure some avg60 (a percentage) past which the host
	// is hot. When the slice's pressure is read it judges the host in
	// place of LoadPerCoreMax (ADR-031). Default DefaultCPUSomeAvg60Max.
	CPUSomeAvg60Max float64 `yaml:"cpu_some_avg60_max,omitempty"`
	// QuietFraction is host.quiet_fraction: once hot, the host stays hot
	// until its pressure (or, without PSI, its load per core) falls
	// below this fraction of the threshold. At most 1. Default
	// DefaultQuietFraction.
	QuietFraction float64 `yaml:"quiet_fraction,omitempty"`
	// OverloadCPUSomeAvg60 is host.overload_cpu_some_avg60: the CPU
	// pressure's some avg60 (a percentage) past which Overloaded is
	// true, in addition to the load-per-core check. Left out or zero,
	// Overloaded never judges by CPU pressure alone.
	OverloadCPUSomeAvg60 float64 `yaml:"overload_cpu_some_avg60,omitempty"`
	// StarveAfter is host.starve_after: how long a build may write
	// nothing to its stream before, on an overloaded host, the node
	// stops it and releases its item (starve.go). Default
	// DefaultStarveAfter, well inside the platform's 45-minute stall.
	StarveAfter time.Duration `yaml:"starve_after,omitempty"`
}

// Effective is h with each zero field at its default; OverloadCPUSomeAvg60
// has none and stays zero (disabled).
func (h HostConfig) Effective() HostConfig {
	if h.LoadPerCoreMax == 0 {
		h.LoadPerCoreMax = DefaultLoadPerCoreMax
	}
	if h.PackageTempMaxC == 0 {
		h.PackageTempMaxC = DefaultPackageTempMaxC
	}
	if h.OverloadPerCore == 0 {
		h.OverloadPerCore = DefaultOverloadPerCore
	}
	if h.CPUSomeAvg60Max == 0 {
		h.CPUSomeAvg60Max = DefaultCPUSomeAvg60Max
	}
	if h.QuietFraction == 0 {
		h.QuietFraction = DefaultQuietFraction
	}
	if h.StarveAfter == 0 {
		h.StarveAfter = DefaultStarveAfter
	}
	return h
}

// Validate refuses a negative threshold, naming the key.
func (h HostConfig) Validate() error {
	if h.LoadPerCoreMax < 0 {
		return errors.New("host.load_per_core_max: cannot be negative")
	}
	if h.PackageTempMaxC < 0 {
		return errors.New("host.package_temp_max_c: cannot be negative")
	}
	if h.OverloadPerCore < 0 {
		return errors.New("host.overload_per_core: cannot be negative")
	}
	if h.CPUSomeAvg60Max < 0 {
		return errors.New("host.cpu_some_avg60_max: cannot be negative")
	}
	if h.QuietFraction < 0 {
		return errors.New("host.quiet_fraction: cannot be negative")
	}
	if h.QuietFraction > 1 {
		return errors.New("host.quiet_fraction: cannot be more than 1")
	}
	if h.OverloadCPUSomeAvg60 < 0 {
		return errors.New("host.overload_cpu_some_avg60: cannot be negative")
	}
	if h.StarveAfter < 0 {
		return errors.New("host.starve_after: cannot be negative")
	}
	return nil
}

// DefaultDiskFreeMinGB is disk.free_min_gb's default: under 10 GiB free
// on the work root the node claims nothing (the disk hour, 2026-10-15).
const DefaultDiskFreeMinGB = 10.0

// DiskConfig is node.yaml's `disk:` block: the free space, in GiB, on
// Path (the work root when left out) under which the node claims nothing
// new (diskfloor.go). A field left out, or zero, takes its default
// (Effective).
type DiskConfig struct {
	FreeMinGB float64 `yaml:"free_min_gb,omitempty"`
	Path      string  `yaml:"path,omitempty"`
}

// Effective is d with FreeMinGB at its default when zero; Path stays as
// given, "" being the work root.
func (d DiskConfig) Effective() DiskConfig {
	if d.FreeMinGB == 0 {
		d.FreeMinGB = DefaultDiskFreeMinGB
	}
	return d
}

// FloorBytes is FreeMinGB in bytes.
func (d DiskConfig) FloorBytes() uint64 {
	return uint64(d.FreeMinGB * (1 << 30))
}

// Validate refuses a negative floor, naming the key.
func (d DiskConfig) Validate() error {
	if d.FreeMinGB < 0 {
		return errors.New("disk.free_min_gb: cannot be negative")
	}
	return nil
}

// hostNum is how a host: threshold is shown in a reload's log line:
// "1.5", "90", "2" — never a trailing ".0".
func hostNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ValidateShares refuses a zero or negative weight, naming the org. Map
// order is arbitrary, so with more than one bad weight only one is
// named; either is a refusal.
func ValidateShares(shares map[string]int) error {
	for org, w := range shares {
		if w <= 0 {
			return fmt.Errorf("shares: %s: must be a positive integer", org)
		}
	}
	return nil
}

// The context budget, in bytes (ADR-015): a session's context — its own
// repos' instructions and INDEX.md with its own goal — aims under
// ContextBytesTarget (past it, one warning naming the file that took it
// there) and never passes DefaultContextBytesMax, the cap: an
// instructions document that would pass it is left off disk and named in
// INDEX.md to fetch first. DefaultDocBytesMax is doc_bytes_max's default,
// kept so node.yaml files that set it still load; it applies to nothing
// on disk since no design document is put there.
const (
	ContextBytesTarget     = 40 * 1024
	DefaultContextBytesMax = 80 * 1024
	DefaultDocBytesMax     = 60 * 1024
)

// ContextConfig is node.yaml's `context:` block, in bytes: bytes_max is
// the most a session's context may put on disk, INDEX.md included.
// doc_bytes_max is still accepted and reloaded, but no longer applies to
// anything on disk (ADR-015). Zero is the default.
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
	// Accounts is the retired `codex.accounts` key, read only to refuse
	// it (Config.Retired): a Codex session runs on an issued OpenAI
	// credential or not at all (ADR 011's amendment 2026-10-02).
	Accounts []any `yaml:"accounts,omitempty"`
	// Models maps the Claude tiers to Codex models (CodexModel).
	Models CodexModels `yaml:"models,omitempty"`
	// Providers are the OpenAI-compatible endpoints an account may run
	// against instead of OpenAI's own, by name (adr-015): a vLLM or
	// SGLang server in front of a model the operator runs. An account
	// naming none runs on OpenAI.
	Providers map[string]CodexProviderConfig `yaml:"providers,omitempty"`
}

// CodexProviderConfig is one entry of `codex.providers`: where the
// endpoint is, which wire it speaks, and what its models cost. The key
// is the provider's name.
type CodexProviderConfig struct {
	// BaseURL is the endpoint including the path a client expects —
	// vLLM and SGLang serve OpenAI's API under /v1.
	BaseURL string `yaml:"base_url"`
	// WireAPI is "chat" or "responses"; empty is "chat", which is what
	// vLLM and SGLang serve.
	WireAPI string `yaml:"wire_api,omitempty"`
	// Prices are list prices per million tokens by model name. A model
	// with none records no cost — right for one the operator runs, and
	// wrong for a hosted endpoint, so name the price when there is a bill.
	Prices map[string]CodexPriceConfig `yaml:"prices,omitempty"`
}

// CodexPriceConfig is a model's list price in dollars per million
// tokens. cached_input and cache_write left out are the input price.
type CodexPriceConfig struct {
	Input       float64 `yaml:"input"`
	CachedInput float64 `yaml:"cached_input,omitempty"`
	CacheWrite  float64 `yaml:"cache_write,omitempty"`
	Output      float64 `yaml:"output"`
}

// Harness is the provider as the Codex adapter takes it.
func (p CodexProviderConfig) Harness(name string) harness.CodexProvider {
	out := harness.CodexProvider{Name: name, BaseURL: p.BaseURL, WireAPI: p.WireAPI}
	if len(p.Prices) > 0 {
		out.Prices = make(map[string]harness.CodexPrice, len(p.Prices))
		for model, pr := range p.Prices {
			cached, write := pr.CachedInput, pr.CacheWrite
			if cached == 0 {
				cached = pr.Input
			}
			if write == 0 {
				write = pr.Input
			}
			out.Prices[model] = harness.CodexPrice{
				Input: pr.Input, CachedInput: cached, CacheWrite: write, Output: pr.Output,
			}
		}
	}
	return out
}

// CodexModels is the Codex model for each Claude tier.
type CodexModels struct {
	Opus   string `yaml:"opus,omitempty"`
	Sonnet string `yaml:"sonnet,omitempty"`
	Haiku  string `yaml:"haiku,omitempty"`
}

// IsZero leaves an empty block out of a saved node.yaml.
func (c CodexConfig) IsZero() bool {
	return c.Binary == "" && len(c.Accounts) == 0 && len(c.Providers) == 0 && c.Models == CodexModels{}
}

// Validate refuses providers Codex could not run against: a name or
// base_url missing, or a wire_api it does not speak. The retired
// `codex.accounts` is Config.Retired's to refuse.
func (c CodexConfig) Validate() error {
	for name, prov := range c.Providers {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("codex.providers: a provider needs a name")
		}
		if strings.TrimSpace(prov.BaseURL) == "" {
			return fmt.Errorf("codex provider %q: base_url is required", name)
		}
		switch prov.WireAPI {
		case "", "chat", "responses":
		default:
			return fmt.Errorf("codex provider %q: wire_api %q: want chat or responses", name, prov.WireAPI)
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

// Scope is one org the node serves, with the node's id there. Login is
// the org's GitHub login, empty for an org with none (grapevine) —
// compare it to a GitHub login (repo owner matching, the orgs
// restriction); Slug is the org's tenant slug, always set, for naming
// the org to a person.
type Scope struct {
	Login     string
	Slug      string
	ID        string
	AccountID string
	NodeID    string
}

// Name is what to show a person for this org: Login, or Slug when
// Login is empty (an org with no GitHub login, like grapevine).
func (s Scope) Name() string {
	if s.Login != "" {
		return s.Login
	}
	return s.Slug
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

// NameOf is an org's Name() — its login, or its slug when it has none —
// or "" when the node does not serve it: the org half of a project's
// `org/project` in a prompt and in the context fetch.
func (s Scopes) NameOf(orgID string) string {
	sc, _ := s.ByOrg(orgID)
	return sc.Name()
}

// Logins lists the orgs served, in order, by their raw GitHub login
// (empty for one with none): for a caller comparing against logins, not
// showing them to a person.
func (s Scopes) Logins() []string {
	out := make([]string, 0, len(s))
	for _, sc := range s {
		out = append(out, sc.Login)
	}
	return out
}

// Names lists the orgs served, in order, by Name(): what to show a
// person.
func (s Scopes) Names() []string {
	out := make([]string, 0, len(s))
	for _, sc := range s {
		out = append(out, sc.Name())
	}
	return out
}

// OwnerKind is "person", "enterprise" or "org": Owner empty or
// "person" is a person's node (bring-your-own infra), "enterprise" is
// shared infra, "org" is one org's own box. LoadConfig has already
// rejected any other value.
func (c *Config) OwnerKind() string {
	switch c.Owner {
	case "enterprise", "org":
		return c.Owner
	}
	return "person"
}

// The contracts a registration can have made (Config.Contract).
const (
	ContractOwner  = "owner"
	ContractPerOrg = "per-org"
)

// OwnedID is the one id an owned node heartbeats under outside the
// loop — `mesh node status`, the rollback reporter — read from the
// contract the last registration saved: NodeID on ContractOwner, else
// empty, so a per-org node (or a node.yaml before `contract:`) that
// kept a node_id is still reached per org alone.
func (c *Config) OwnedID() string {
	if c.ContractKind() != ContractOwner {
		return ""
	}
	return c.NodeID
}

// ContractKind is Contract, an empty one read as ContractPerOrg.
func (c *Config) ContractKind() string {
	if c.Contract == ContractOwner {
		return ContractOwner
	}
	return ContractPerOrg
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
// node its id in each. A name is matched against an org's Login or its
// TenantSlug, so an org with no GitHub login (grapevine) is named by its
// slug — the same rule ScopesFromAnswer applies. Orgs the config names
// that the owner is not in are an error: the node would never see their
// items.
func ResolveScopes(ctx context.Context, gw interface {
	MyOrgs(ctx context.Context) ([]gateway.Org, error)
}, c *Config) (Scopes, error) {
	orgs, err := gw.MyOrgs(ctx)
	if err != nil {
		return nil, fmt.Errorf("my orgs: %w", err)
	}
	want := map[string]bool{}
	for _, o := range c.Orgs {
		want[strings.ToLower(strings.TrimSpace(o))] = true
	}
	// want stays whole for the entire loop: found records which of it
	// matched, so a match never lifts the restriction for later orgs.
	found := map[string]bool{}
	var scopes Scopes
	for _, o := range orgs {
		login, slug := strings.ToLower(o.Login), strings.ToLower(o.TenantSlug)
		if len(want) > 0 && !want[login] && !want[slug] {
			continue
		}
		id, err := c.NodeIDIn(o.ID)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, Scope{Login: o.Login, Slug: o.TenantSlug, ID: o.ID, AccountID: o.AccountID, NodeID: id})
		found[login] = true
		found[slug] = true
	}
	if missing := missingOrgs(want, found); len(missing) > 0 {
		return nil, fmt.Errorf("not a member of %s (the node.yaml orgs list names them)", strings.Join(missing, ", "))
	}
	if len(scopes) == 0 {
		return nil, errors.New("the token's person belongs to no claimed org")
	}
	return scopes, nil
}

// ScopesFromAnswer turns the platform's NodeAnswer (registerNode or
// heartbeatNode) into Scopes: one per a.Scopes entry, every one sharing
// the node's single NodeID (a.NodeID). restrict is node.yaml's orgs:
// list, matched case-insensitively against a scope's Login or Slug
// (empty keeps every org the answer names); a restricted name the
// answer does not contain is an error listing the missing names, the
// same rule ResolveScopes applies. An answer with no org left to serve
// is an error too.
func ScopesFromAnswer(a *gateway.NodeAnswer, restrict []string) (Scopes, error) {
	want := map[string]bool{}
	for _, o := range restrict {
		want[strings.ToLower(strings.TrimSpace(o))] = true
	}
	found := map[string]bool{}
	var scopes Scopes
	for _, sa := range a.Scopes {
		login, slug := strings.ToLower(sa.Login), strings.ToLower(sa.Slug)
		if len(want) > 0 && !want[login] && !want[slug] {
			continue
		}
		found[login] = true
		found[slug] = true
		scopes = append(scopes, Scope{Login: sa.Login, Slug: sa.Slug, ID: sa.OrgID, AccountID: sa.AccountID, NodeID: a.NodeID})
	}
	if missing := missingOrgs(want, found); len(missing) > 0 {
		return nil, fmt.Errorf("not a member of %s (the node.yaml orgs list names them)", strings.Join(missing, ", "))
	}
	if len(scopes) == 0 {
		return nil, errors.New("the platform answered with no org to serve")
	}
	return scopes, nil
}

// missingOrgs is every name in want that found does not hold, sorted.
func missingOrgs(want, found map[string]bool) []string {
	var missing []string
	for o := range want {
		if !found[o] {
			missing = append(missing, o)
		}
	}
	sort.Strings(missing)
	return missing
}

// Owner is the node's owner, read off a NodeAnswer: a person, an
// enterprise or an org, its id and its display name.
type Owner struct {
	Kind string
	ID   string
	Name string
}

// OwnerFrom reads Owner off the platform's NodeAnswer.
func OwnerFrom(a *gateway.NodeAnswer) Owner {
	return Owner{Kind: a.Owner.Kind, ID: a.Owner.ID, Name: a.Owner.Name}
}

// String is what to show a person: "owned by Chris" / "owned by
// anythink" — Name, falling back to "<kind> <id>" when Name is empty.
func (o Owner) String() string {
	name := o.Name
	if name == "" {
		name = strings.TrimSpace(o.Kind + " " + o.ID)
	}
	return "owned by " + name
}

// Who is the owner's name alone — "Chris", "anythink (enterprise)",
// "Grapevine (org)", an enterprise or an org named with its kind —
// falling back to "<kind> <id>" when Name is empty: what the log lines
// name.
func (o Owner) Who() string {
	who := strings.TrimPrefix(o.String(), "owned by ")
	if (o.Kind == "enterprise" || o.Kind == "org") && o.Name != "" {
		who += " (" + o.Kind + ")"
	}
	return who
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
	// Claim is node.yaml's `claim:` block; nil keeps the loop's.
	Claim *ClaimConfig
	// Orgs is node.yaml's orgs restriction, read when OrgsRead: an owned
	// node applies it to the platform's answer (TakeAnswer).
	Orgs     []string
	OrgsRead bool
	// Shares is node.yaml's `shares:` weights, read when SharesRead; nil
	// is no weights set (equal shares), distinct from not having read
	// node.yaml at all this tick.
	Shares     map[string]int
	SharesRead bool
	// Host is node.yaml's `host:` block, defaults filled; nil keeps the
	// loop's.
	Host *HostConfig
	// Disk is node.yaml's `disk:` block; nil keeps the loop's.
	Disk *DiskConfig
	// Takes is node.yaml's `takes:` block; nil keeps the loop's.
	Takes *Takes
}

// Spending is what the node may spend on: node.yaml's project budgets
// and per-kind, per-model session budgets, read again every tick like
// the capacity. The accounts are the platform's (Known), never node.yaml's.
type Spending struct {
	ProjectBudgets map[string]float64
	// Budgets is node.yaml's `budgets:` (ADR 012).
	Budgets map[string]map[string]float64
}

// Reloader is Loop.Reload for a node started on cfg and scopes: every
// tick it reads node.yaml again through load (LoadConfig when nil) for
// the capacity, the limits thresholds, the accounts and project budgets,
// the context budget, the claim limits, the shares weights and the orgs
// restriction, and asks MyOrgs for the orgs
// the owner belongs to. An org seen for the first time is registered in
// through register (a Node there, as start does) before it is served —
// one that fails to register is tried again next tick; an org no longer
// listed, or no longer in the restriction, is dropped. A node.yaml that
// cannot be read keeps the last good capacity, limits, accounts, context,
// claim limits, shares and restriction; a MyOrgs that fails, or leaves
// nothing to serve, keeps the orgs as they were — and a node.yaml read
// well is still returned, so the operator's edit does not wait on the
// gateway. The reasons a reload could not be applied come back as one
// error, joined with "; " (each cause still wrapped), which the loop logs
// as one `reload config:` line per tick while the failure lasts.
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
			out = reloadedFrom(c)
			restrict = c.Orgs
		}
		orgs, err := gw.MyOrgs(ctx)
		if err != nil {
			return out, joinReasons(append(errs, fmt.Errorf("my orgs: %w", err)))
		}
		want := map[string]bool{}
		for _, o := range restrict {
			want[strings.ToLower(strings.TrimSpace(o))] = true
		}
		var served Scopes
		for _, o := range orgs {
			login, slug := strings.ToLower(o.Login), strings.ToLower(o.TenantSlug)
			if len(want) > 0 && !want[login] && !want[slug] {
				continue
			}
			id, err := cfg.NodeIDIn(o.ID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			sc := Scope{Login: o.Login, Slug: o.TenantSlug, ID: o.ID, AccountID: o.AccountID, NodeID: id}
			if !registered[id] && register != nil {
				if err := register(ctx, sc); err != nil {
					errs = append(errs, fmt.Errorf("register in %s: %w", sc.Name(), err))
					continue
				}
			}
			registered[id] = true
			served = append(served, sc)
		}
		if len(served) == 0 {
			return out, joinReasons(append(errs, errors.New("no org left to serve (keeping the orgs as they were)")))
		}
		out.Scopes = served
		return out, joinReasons(errs)
	}
}

// reasons is why a reload could not be applied: every cause, joined
// with "; " on one line (errors.Join's newline would split the log
// line), each still reachable through errors.Is and errors.As.
type reasons []error

func (r reasons) Error() string {
	s := make([]string, len(r))
	for i, err := range r {
		s[i] = err.Error()
	}
	return strings.Join(s, "; ")
}

func (r reasons) Unwrap() []error { return r }

// joinReasons is nil for no reasons, else the reasons as one error.
func joinReasons(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return reasons(errs)
}

// reloadedFrom is what a re-read node.yaml gives the loop: the
// capacity, the limits, the accounts and budgets, the context budget,
// the claim limits and the orgs restriction.
func reloadedFrom(c *Config) Reloaded {
	limits, budget, claim, host, disk, takes := c.Limits, c.Context, c.Claim, c.Host, c.Disk, c.Takes
	return Reloaded{Capacity: c.Capacity, Limits: &limits, Context: &budget, Claim: &claim,
		Spending: &Spending{ProjectBudgets: c.ProjectBudgets, Budgets: c.Budgets},
		Orgs:     c.Orgs, OrgsRead: true,
		Shares: c.Shares, SharesRead: true,
		Host:  &host,
		Disk:  &disk,
		Takes: &takes}
}

// OwnedReloader is Loop.Reload for a node registered once with its
// owner (ADR-015, Registration and the loop): every tick it reads
// node.yaml again through load (LoadConfig when nil) for what Reloader
// reads from it — the capacity, the limits, the accounts, the context,
// the claim limits and the orgs restriction — but never asks MyOrgs and
// never registers: the orgs served come only from the platform's answer
// to each heartbeat (TakeAnswer), restricted there.
func OwnedReloader(load func() (*Config, error)) func(context.Context) (Reloaded, error) {
	if load == nil {
		load = LoadConfig
	}
	return func(context.Context) (Reloaded, error) {
		c, err := load()
		if err != nil {
			return Reloaded{}, fmt.Errorf("node.yaml (keeping the last good capacity and limits): %w", err)
		}
		return reloadedFrom(c), nil
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

// LoadConfig reads ~/.mesh/node.yaml, refusing a retired key
// (Config.Retired) before anything else is judged.
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
	if err := c.Retired(); err != nil {
		return nil, err
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
	if len(c.Orgs) == 0 && c.OrgLogin != "" && c.Owner != "org" {
		c.Orgs = []string{c.OrgLogin} // an older, single-org registration
	}
	for p, b := range c.ProjectBudgets {
		if b < 0 {
			return nil, fmt.Errorf("%s: project_budgets[%s]: a budget cannot be negative", path, p)
		}
	}
	if err := c.Codex.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Claim.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Takes.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Context.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := ValidateShares(c.Shares); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Disk.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Host.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.ValidationCacheMaxBytes < 0 {
		return nil, fmt.Errorf("%s: validation_cache_max_bytes: cannot be negative", path)
	}
	switch c.Owner {
	case "", "person", "enterprise", "org":
	default:
		return nil, fmt.Errorf(`%s: owner: %q is not "person", "enterprise" or "org"`, path, c.Owner)
	}
	if err := c.ValidateOwner(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// ValidateOwner checks Owner, Enterprise and Org agree: an enterprise
// owner must name the enterprise it belongs to (`enterprise:`), an org
// owner the org (`org:`), and each is only ever set for its own kind.
func (c *Config) ValidateOwner() error {
	if c.Owner == "enterprise" && strings.TrimSpace(c.Enterprise) == "" {
		return errors.New("owner: enterprise names no enterprise — add `enterprise: <slug or id>` (the enterprise that owns this node, e.g. enterprise: anythink)")
	}
	if c.Enterprise != "" && c.Owner != "enterprise" {
		return errors.New("enterprise: is set but owner: is not enterprise — set owner: enterprise or remove enterprise:")
	}
	if c.Owner == "org" && strings.TrimSpace(c.OrgLogin) == "" {
		return errors.New("owner: org names no org — add `org: <slug or id>` (the org that owns this node, e.g. org: grapevine)")
	}
	// An older single-org registration wrote org: beside account_id;
	// it still loads, read as Orgs of one.
	if c.OrgLogin != "" && c.Owner != "org" && c.AccountID == "" {
		return errors.New("org: is set but owner: is not org — set owner: org or remove org:")
	}
	return nil
}

// ErrRetired is the refusal of a node.yaml key the node no longer
// honours: a node runs sessions only on credentials the platform
// issues (goal a-session-never-runs-on-the-machines-own-login,
// adr-005's 2026-10 amendment).
var ErrRetired = errors.New("retired")

// retiredKey is the refusal of one retired key, naming it and where
// its credentials go instead.
func retiredKey(key string) error {
	return fmt.Errorf("node.yaml: %s is %w — a node runs sessions only on credentials the platform issues; add them on the console's Credentials page and attach them to the project (see README, 'Credentials come from the platform')", key, ErrRetired)
}

// Retired refuses a node.yaml that lists `accounts:` or
// `codex.accounts`: both named the machine's own logins and keys, and
// a node never runs a session on those. An empty list, or neither key,
// is no refusal.
func (c *Config) Retired() error {
	if len(c.Accounts) > 0 {
		return retiredKey("accounts:")
	}
	if len(c.Codex.Accounts) > 0 {
		return retiredKey("codex.accounts")
	}
	return nil
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

// ReposRoot is ~/.mesh/repos, beside the work root: the node's one clone
// per repo, each item's checkout a worktree of it (mesh ADR 017).
func ReposRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "repos"), nil
}

// StoreRoot is ~/.mesh/store, the node's package store and cache
// (mesh-specs ADR 017 §3), on the filesystem of WorkRoot.
func StoreRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "store"), nil
}

// PackageStores are the node's one pnpm store and one npm cache under
// root, which every session of the node installs against.
func PackageStores(root string) sandbox.Stores {
	return sandbox.Stores{Pnpm: filepath.Join(root, "pnpm"), Npm: filepath.Join(root, "npm")}
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
