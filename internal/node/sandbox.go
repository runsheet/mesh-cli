package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/sandbox"
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
	// Image is the node image; empty is ghcr.io/projectmesh-io/mesh-node
	// at the node's version.
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
	s := sandbox.Config{
		Image:          c.Image,
		Limits:         sandbox.Limits{CPUs: c.CPUs, Memory: c.Memory, Pids: c.Pids},
		ServicesLimits: sandbox.Limits{CPUs: c.ServicesCPUs, Memory: c.ServicesMemory},
		Registries:     c.Registries,
		Network:        sandbox.DefaultNetwork(),
		ServiceTimeout: c.ServiceTimeout,
	}
	if s.Image == "" {
		s.Image = sandbox.DefaultImage(version)
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
	cfg := c.Settings(version)
	if logf != nil {
		logf("sandbox: on — %s, image %s, agent %g cpus / %s / %d pids", ver, cfg.Image, cfg.Limits.CPUs, cfg.Limits.Memory, cfg.Limits.Pids)
	}
	return &sandbox.Sandbox{Runtime: rt, Config: cfg}, &gateway.SandboxInfo{Mode: SandboxOn, Runtime: ver, Image: cfg.Image}, nil
}

// LimitReason is what a session killed by its sandbox's limits is
// released with and what the node's heartbeat says of it.
func LimitReason(label string, le *sandbox.LimitError) string {
	return fmt.Sprintf("sandbox limit: item %s killed by its %s limit (%s); released for a node with more room", label, le.Limit, le.Value)
}
