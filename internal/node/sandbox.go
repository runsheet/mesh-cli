package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/validate"
)

// Sandbox modes of node.yaml's `sandbox:`.
const (
	SandboxOn  = "on"
	SandboxOff = "off"
)

// SandboxConfig is node.yaml's `sandbox:` (ADR 007): whether sessions
// run in their own pods, and the pods' image, limits and the registries
// their services may come from. `sandbox: off` (or `on`) alone is the
// mode; a mode left out is on when the runtime answers, else off.
type SandboxConfig struct {
	// Mode is on, off, or "" — on when `podman --version` answers.
	Mode string `yaml:"mode,omitempty"`
	// CPUs, Memory and Pids cap the agent's container (default 2, 6g,
	// 4096).
	CPUs   float64 `yaml:"cpus,omitempty"`
	Memory string  `yaml:"memory,omitempty"`
	Pids   int     `yaml:"pids,omitempty"`
	// ServicesCPUs and ServicesMemory are the services' budget together
	// (default 2, 4g).
	ServicesCPUs   float64 `yaml:"services_cpus,omitempty"`
	ServicesMemory string  `yaml:"services_memory,omitempty"`
	// Registries are where service images may come from (default
	// docker.io, ghcr.io, *.pkg.dev, quay.io).
	Registries []string `yaml:"registries,omitempty"`
	// Image is the node image; empty is ghcr.io/runsheet/mesh-node
	// at the node's version, `build` that same tag built on this machine
	// from the embedded Containerfile, anything else used as given.
	Image string `yaml:"image,omitempty"`
	// ServiceTimeout bounds the wait for a session's services' health
	// checks before its agent starts (default 120s).
	ServiceTimeout time.Duration `yaml:"service_timeout,omitempty"`
}

// UnmarshalYAML takes `sandbox: on|off` or the mapping.
func (c *SandboxConfig) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var mode string
		if err := n.Decode(&mode); err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
		*c = SandboxConfig{Mode: mode}
		return c.Validate()
	}
	type plain SandboxConfig
	var p plain
	if err := n.Decode(&p); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	*c = SandboxConfig(p)
	return c.Validate()
}

// IsZero leaves an unset block out of a saved node.yaml.
func (c SandboxConfig) IsZero() bool {
	return c.Mode == "" && c.CPUs == 0 && c.Memory == "" && c.Pids == 0 && c.ServicesCPUs == 0 && c.ServicesMemory == "" && len(c.Registries) == 0 && c.Image == "" && c.ServiceTimeout == 0
}

// Validate refuses a mode other than on or off and negative limits.
func (c SandboxConfig) Validate() error {
	switch strings.ToLower(c.Mode) {
	case "", SandboxOn, SandboxOff:
	default:
		return fmt.Errorf("sandbox.mode %q: want on or off", c.Mode)
	}
	if c.CPUs < 0 || c.ServicesCPUs < 0 || c.Pids < 0 || c.ServiceTimeout < 0 {
		return errors.New("sandbox: cpus, services_cpus, pids and service_timeout cannot be negative")
	}
	return nil
}

// Settings is the sandbox's configuration with the defaults filled in,
// for a node at version.
func (c SandboxConfig) Settings(version string) sandbox.Config {
	image, _, err := sandbox.ImageRef(c.Image, version)
	if err != nil {
		image = sandbox.DefaultImage(version)
	}
	s := sandbox.Config{
		Image:          image,
		Limits:         sandbox.Limits{CPUs: c.CPUs, Memory: c.Memory, Pids: c.Pids},
		ServicesLimits: sandbox.Limits{CPUs: c.ServicesCPUs, Memory: c.ServicesMemory},
		Registries:     c.Registries,
		Network:        sandbox.DefaultNetwork(),
		ServiceTimeout: c.ServiceTimeout,
	}
	if s.Limits.CPUs == 0 {
		s.Limits.CPUs = sandbox.DefaultCPUs
	}
	if s.Limits.Memory == "" {
		s.Limits.Memory = sandbox.DefaultMemory
	}
	if s.Limits.Pids == 0 {
		s.Limits.Pids = sandbox.DefaultPids
	}
	if s.ServicesLimits.CPUs == 0 {
		s.ServicesLimits.CPUs = sandbox.DefaultServicesCPUs
	}
	if s.ServicesLimits.Memory == "" {
		s.ServicesLimits.Memory = sandbox.DefaultServicesMemory
	}
	if len(s.Registries) == 0 {
		s.Registries = append([]string(nil), sandbox.DefaultRegistries...)
	}
	if s.ServiceTimeout == 0 {
		s.ServiceTimeout = sandbox.DefaultServiceTimeout
	}
	return s
}

// OpenSandbox decides the node's sandbox at start: nil with mode off for
// `sandbox: off`; on a runtime that answers, the sandbox and mode on;
// with the mode left out and no runtime, nil and mode off with a log
// line. An explicit `sandbox: on` with no runtime is an error: the node
// fails closed and never runs a session unsandboxed on its own (ADR 007).
func OpenSandbox(ctx context.Context, c SandboxConfig, rt sandbox.Runtime, version string, logf func(string, ...any)) (*sandbox.Sandbox, *gateway.SandboxInfo, error) {
	mode := strings.ToLower(c.Mode)
	if mode == SandboxOff {
		return nil, &gateway.SandboxInfo{Mode: SandboxOff}, nil
	}
	ver, err := rt.Version(ctx)
	if err != nil {
		if mode == SandboxOn {
			return nil, nil, fmt.Errorf("sandbox: on, but the runtime does not answer (%v): install podman, or set `sandbox: off` in node.yaml", err)
		}
		if logf != nil {
			logf("sandbox: off — the runtime does not answer (%v); sessions run on the host as before. Install podman, or set `sandbox: off` in node.yaml to say so", err)
		}
		return nil, &gateway.SandboxInfo{Mode: SandboxOff}, nil
	}
	// no store of the node's own: each session's pod mounts its
	// connection's (orgSandbox, ADR-039)
	cfg := c.Settings(version)
	if logf != nil {
		logf("sandbox: on — %s, image %s, agent %g cpus / %s / %d pids", ver, cfg.Image, cfg.Limits.CPUs, cfg.Limits.Memory, cfg.Limits.Pids)
		// under systemd (INVOCATION_ID set), a unit whose KillMode is not
		// process kills every pod with the node on a restart or self-update
		if os.Getenv("INVOCATION_ID") != "" {
			if mode, ok := SystemdKillMode(ctx, nil); ok && mode != SafeKillMode {
				logf("sandbox: WARNING the service's unit has KillMode=%s: a restart or a self-update ends every running session with the node; add KillMode=%s under [Service] in %s and run systemctl --user daemon-reload", mode, SafeKillMode, SystemdUnitPath("~"))
			}
		}
	}
	return &sandbox.Sandbox{Runtime: rt, Config: cfg, Logf: logf}, &gateway.SandboxInfo{Mode: SandboxOn, Runtime: ver, Image: cfg.Image}, nil
}

// SweepSandbox is the sandbox's start-up sweep (ADR-007's amendment
// 2026-10-10): run after the sessions of the process before this one are
// adopted and before the first tick, it removes the HOMEs, pods and
// containers sandboxed sessions left, keeping every HOME a running
// container mounts and — whatever their state — the pod, its containers
// and the HOME of every found sandboxed session, which adoption reads,
// validates in and removes at its end (mesh-cli ADR 009). It logs one
// line of what it did. No sandbox sweeps nothing.
//
// Run again on the quartermaster's cadence (ScratchCheck, ADR-034) it
// also keeps every session this process has open (sandbox.Sandbox.Live):
// a validation between two steps or a session between Open and its
// agent's first container has a pod with only its infra running, which
// is exactly what the sweep would otherwise remove — and did, on the
// dl360 on 2026-10-17, 184 validations in five hours dying as "step
// exited 137" or "no pod with ID".
func SweepSandbox(ctx context.Context, sb *sandbox.Sandbox, found []FoundSession, logf func(string, ...any)) sandbox.SweepResult {
	if sb == nil || sb.Runtime == nil {
		return sandbox.SweepResult{}
	}
	keep := sb.Live()
	for _, s := range found {
		if s.Sandboxed() {
			keep.Pods = append(keep.Pods, s.State.Pod)
			keep.Homes = append(keep.Homes, s.State.Home)
		}
	}
	res, err := sandbox.SweepKeeping(ctx, sb.Runtime, sb.TempDir, keep)
	if err != nil {
		logf("sandbox: sweep at start: %s", strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	logf("%s", res.Line())
	return res
}

// EnsureImage makes the node image of version — the one node.yaml's
// `sandbox.image` names for it — present on this machine, before the
// node drains onto that version (mesh-specs ADR 007): pulled, or with
// `sandbox.image: build` built from the embedded Containerfile; the image
// of running, which the node's sessions still use, is kept, and every
// other mesh-node tag removed. `sandbox: off` asks nothing, nor does a
// mode left out on a machine whose runtime does not answer (the node
// there runs off); both are Image.Skipped. Every other failure is the
// error: the update does not go ahead on it. A pull is on the registry
// credential broker gives (RegistryBrokerOf; nil pulls on podman's own
// login), by --authfile (sandbox.EnsureImage).
func (c SandboxConfig) EnsureImage(ctx context.Context, run sandbox.Runner, version, running string, broker sandbox.RegistryBroker) (sandbox.Image, error) {
	mode := strings.ToLower(c.Mode)
	if mode == SandboxOff {
		return sandbox.Image{Skipped: "sandbox: off"}, nil
	}
	if out, err := run(ctx, "podman", "--version"); err != nil {
		reason := strings.TrimSpace(string(out))
		if reason == "" {
			reason = err.Error()
		}
		if mode == SandboxOn {
			return sandbox.Image{}, fmt.Errorf("sandbox: on, but the runtime does not answer (%s)", reason)
		}
		return sandbox.Image{Skipped: "the runtime does not answer (" + reason + "); the sandbox is off"}, nil
	}
	ref, build, err := sandbox.ImageRef(c.Image, version)
	if err != nil {
		return sandbox.Image{}, err
	}
	var keep []string
	if running != "" {
		keep = append(keep, c.Settings(running).Image)
	}
	if build {
		return sandbox.BuildImage(ctx, run, ref, version, keep...)
	}
	return sandbox.EnsureImage(ctx, run, ref, broker, keep...)
}

// RegistryTokens is the platform's read of a node's registry credential
// (gateway.Client.NodeRegistryToken).
type RegistryTokens interface {
	NodeRegistryToken(ctx context.Context, registry string) (*gateway.RegistryToken, error)
}

// RegistryBrokerOf is the node image pull's broker on the platform: the
// credential nodeRegistryToken gives for the image's registry. A
// platform without the read, or with none for the registry, gives none,
// and the pull goes on podman's own login as before (ADR-007's amendment
// 2026-10-13); any other error is the broker's, named if the pull fails.
func RegistryBrokerOf(p RegistryTokens) sandbox.RegistryBroker {
	if p == nil {
		return nil
	}
	return func(ctx context.Context, registry string) (*sandbox.RegistryCredential, error) {
		tok, err := p.NodeRegistryToken(ctx, registry)
		if gateway.IsUnsupported(err) {
			return nil, nil
		}
		if err != nil || tok == nil {
			return nil, err
		}
		return &sandbox.RegistryCredential{Username: tok.Username, Secret: tok.Token}, nil
	}
}

// LimitReason is what a session killed by its sandbox's limits is
// released with and what the node's heartbeat says of it.
func LimitReason(label string, le *sandbox.LimitError) string {
	return fmt.Sprintf("sandbox limit: item %s killed by its %s limit (%s); released for a node with more room", label, le.Limit, le.Value)
}

// SandboxRequiredPrefix opens the reason a session a multi-org node
// would have run on the host is handed back with
// (validate.SandboxRequired, ADR-030).
const SandboxRequiredPrefix = string(validate.SandboxRequired) + ": "

// ErrHostServesSeveralOrgs is what every HostFallbackError Is.
var ErrHostServesSeveralOrgs = errors.New("a node serving several orgs runs no session on the host")

// HostFallbackError is a session refused because the node serves two or
// more orgs and has no sandbox to run it in (`sandbox: off`, or a mode
// left out on a machine whose runtime does not answer): on the host a
// session runs as the node user and can read every org's checkouts,
// clones, kept sessions, stores and MCP data (mesh-cli ADR-034,
// Refusals). Org is the session's org, Orgs every one the node serves,
// Path the checkout the session would have run in.
type HostFallbackError struct {
	Org  string
	Orgs []string
	Path string
}

func (e *HostFallbackError) Error() string {
	return fmt.Sprintf("sandbox: off on a node serving %d orgs (%s); a session of org %s in %s on the host could read the others' files: refused",
		len(e.Orgs), strings.Join(e.Orgs, ", "), e.Org, e.Path)
}

// Is makes it ErrHostServesSeveralOrgs.
func (e *HostFallbackError) Is(target error) bool { return target == ErrHostServesSeveralOrgs }

// EnvironmentKind is validate.SandboxRequired: the node's condition,
// never the item's.
func (e *HostFallbackError) EnvironmentKind() validate.EnvironmentKind {
	return validate.SandboxRequired
}

// HostFallback refuses a session of org (an org id) in dir on a node
// with no sandbox (sb nil) that serves two or more orgs, naming the org,
// the dir and the orgs served; a sandboxed node, or one serving one
// org, is nil.
func HostFallback(sb *sandbox.Sandbox, scopes Scopes, orgID, dir string) error {
	if sb != nil || len(scopes.Names()) < 2 {
		return nil
	}
	org := scopes.NameOf(orgID)
	if org == "" {
		org = orgID
	}
	return &HostFallbackError{Org: org, Orgs: scopes.Names(), Path: dir}
}

// HostFallbackReason is the reason a refused session is handed back
// with: `sandbox_required: <the refusal>`.
func HostFallbackReason(err error) string {
	return truncate(validate.WithKind(validate.SandboxRequired, err.Error()), 2000)
}

// hostFallbackSaid says the refusal in node.log once per run; each
// item's release line still names it.
var hostFallbackSaid sync.Once

// noteHostFallback logs the refusal once per run.
func noteHostFallback(err error, logf func(string, ...any)) {
	hostFallbackSaid.Do(func() {
		logf("%v; every session is handed back until the sandbox is on (set `sandbox: on` and install podman, or serve one org)", err)
	})
}

// ReleaseOnHostFallback hands back an item a multi-org node with no
// sandbox will not run (HostFallback): ReleaseWorkItem, attempts
// unchanged, with `sandbox_required: <the refusal>` as the reason — the
// node's condition, never the item's failure (ADR-024, ADR-030). The
// refusal is said once per run in node.log (noteHostFallback), each
// item's release in one short line. A refused release fails the item
// with the same reason, as ReleaseOnWorkspace does; a cancelled ctx
// sends nothing. It answers the error the session ends with.
func ReleaseOnHostFallback(ctx context.Context, gw Gateway, itemID, nodeID, label string, cause error, logf func(string, ...any)) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	noteHostFallback(cause, logf)
	reason := HostFallbackReason(cause)
	wrapped := fmt.Errorf("%s%w", SandboxRequiredPrefix, cause)
	err := gw.ReleaseWorkItem(ctx, itemID, nodeID, reason)
	if err == nil {
		logf("item %s: released, no attempt charged (%s)", label, validate.SandboxRequired)
		return wrapped
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return wrapped
	}
	logf("item %s: %s release refused (%v); failed instead", label, validate.SandboxRequired, err)
	if err := gw.FailWorkItem(ctx, itemID, reason); err != nil && !errors.Is(err, context.Canceled) {
		logf("item %s: fail: %v", label, err)
	}
	return wrapped
}

// releaseHostFallback is ReleaseOnHostFallback for the Runner's item,
// then the hold that keeps this node off it for WorkspaceReleaseHold,
// so a refused item does not spin between claim and release.
func (r *Runner) releaseHostFallback(ctx context.Context, item gateway.WorkItem, label string, cause error) error {
	sc, _ := r.Scopes.ByOrg(item.OrgID)
	err := ReleaseOnHostFallback(ctx, r.GW, item.ID, sc.NodeID, label, cause, r.logf)
	if ctx.Err() == nil {
		r.Declines.HoldFor(item.ID, HostFallbackReason(cause), time.Now(), WorkspaceReleaseHold)
	}
	return err
}
