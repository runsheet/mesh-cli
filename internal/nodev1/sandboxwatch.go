package nodev1

import (
	"context"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// nodeYAMLSandboxLimits is node.yaml's own top-level `sandbox: {cpus,
// memory, pids}` section (ADR-007 amendment, 2026-11-03), the same flat
// keys the legacy internal/node.SandboxConfig reads — nodev1 reads them
// on its own, never through internal/node, which it may not import
// (guard_test.go).
type nodeYAMLSandboxLimits struct {
	Sandbox struct {
		CPUs   float64 `yaml:"cpus"`
		Memory string  `yaml:"memory"`
		Pids   int     `yaml:"pids"`
	} `yaml:"sandbox"`
}

// ReadSandboxLimitsConfig reads node.yaml's top-level `sandbox` section
// into a sandbox.Limits: false with no error when the file does not
// exist or names none of cpus, memory or pids.
func ReadSandboxLimitsConfig(path string) (sandbox.Limits, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sandbox.Limits{}, false, nil
		}
		return sandbox.Limits{}, false, err
	}
	var cfg nodeYAMLSandboxLimits
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return sandbox.Limits{}, false, err
	}
	if cfg.Sandbox.CPUs == 0 && cfg.Sandbox.Memory == "" && cfg.Sandbox.Pids == 0 {
		return sandbox.Limits{}, false, nil
	}
	return sandbox.Limits{CPUs: cfg.Sandbox.CPUs, Memory: cfg.Sandbox.Memory, Pids: cfg.Sandbox.Pids}, true, nil
}

// WatchSandboxLimits is the node.yaml sandbox-limits watcher (ADR-007
// amendment, 2026-11-03, "per-pod sandbox limits are read live, at pod
// creation"): it rereads node.yaml's `sandbox` section with read every
// poll, and at once whenever reload fires, calling r.Pods.SetAgentLimits
// whenever what it read differs from r.Pods' current AgentLimits — so an
// operator who edits node.yaml and signals the process (wired to SIGHUP
// by `mesh node run`, the same signal WatchCapacity answers) changes
// every pod created from then on, while a pod already running keeps the
// limits it was given (Session.SetLimits snapshots them once, at its own
// creation). A read that errors changes nothing and is logged once; one
// that finds no sandbox limits changes nothing silently, since node.yaml
// may simply name none. With no Pods sandbox the watcher does nothing,
// since there is nothing to set.
func (r *Runtime) WatchSandboxLimits(ctx context.Context, read func() (sandbox.Limits, bool, error), poll time.Duration, reload <-chan struct{}) {
	if read == nil || r.Pods == nil {
		return
	}
	if poll <= 0 {
		poll = time.Minute
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	apply := func() {
		l, ok, err := read()
		if err != nil {
			r.logf("nodev1: sandbox limits: node.yaml: %v", err)
			return
		}
		if !ok {
			return
		}
		if r.Pods.AgentLimits() == l {
			return
		}
		r.Pods.SetAgentLimits(l)
	}
	apply()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			apply()
		case <-reload:
			apply()
		}
	}
}
