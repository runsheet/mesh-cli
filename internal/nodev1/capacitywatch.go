package nodev1

import (
	"context"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// nodeYAMLCapacity is node.yaml's own `v1: {capacity: {cpus, mem_gb,
// slots}}` section (ADR-044 amendment, 2026-11-03): the v1 node reads it
// on its own, never through the legacy internal/node.Config, which
// nodev1 may not import (guard_test.go); a v1 node run beside a legacy
// one on the same file reads only its own section. Its own yaml tags,
// never meshapi.Capacity's json ones, which yaml.v3 does not read. Each
// field is a pointer so a partial block (as `mesh node set` writes, and
// as the flags-become-defaults startup rule reads, ADR-044 amendment,
// 2026-11-03) is told apart from one that names zero.
type nodeYAMLCapacity struct {
	V1 struct {
		Capacity *nodeYAMLCapacityFields `yaml:"capacity"`
	} `yaml:"v1"`
}

type nodeYAMLCapacityFields struct {
	CPUs  *float64 `yaml:"cpus"`
	MemGB *float64 `yaml:"mem_gb"`
	Slots *int     `yaml:"slots"`
}

// ReadCapacityConfig reads node.yaml's `v1.capacity`: false with no
// error when the file does not exist or names no capacity. A field the
// block does not set reads as zero, as it always has.
func ReadCapacityConfig(path string) (meshapi.Capacity, bool, error) {
	cfg, ok, err := readCapacityYAML(path)
	if err != nil || !ok {
		return meshapi.Capacity{}, ok, err
	}
	return meshapi.Capacity{CPUs: floatOr(cfg.CPUs, 0), MemGB: floatOr(cfg.MemGB, 0), Slots: intOr(cfg.Slots, 0)}, true, nil
}

// readCapacityYAML is node.yaml's `v1.capacity` block as the file names
// it, pointers left nil for a field the block leaves out; false with no
// error when the file does not exist or names no capacity block at all.
func readCapacityYAML(path string) (nodeYAMLCapacityFields, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nodeYAMLCapacityFields{}, false, nil
		}
		return nodeYAMLCapacityFields{}, false, err
	}
	var cfg nodeYAMLCapacity
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nodeYAMLCapacityFields{}, false, err
	}
	if cfg.V1.Capacity == nil {
		return nodeYAMLCapacityFields{}, false, nil
	}
	return *cfg.V1.Capacity, true, nil
}

func floatOr(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// WatchCapacity is the node.yaml watcher (ADR-044 amendment,
// 2026-11-03): it rereads the node's capacity with read every poll, and
// at once whenever reload fires, calling SetCapacity whenever what it
// read differs from the node's current total — so an operator who
// edits node.yaml and signals the process (wired to SIGHUP by `mesh
// node run`) sees the new capacity take effect without a restart, the
// same as the poll finds it on its own clock. A read that errors
// changes nothing and is logged once; one that finds no capacity
// changes nothing silently, since node.yaml may simply name none.
func (r *Runtime) WatchCapacity(ctx context.Context, read func() (meshapi.Capacity, bool, error), poll time.Duration, reload <-chan struct{}) {
	if read == nil {
		return
	}
	if poll <= 0 {
		poll = time.Minute
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	apply := func() {
		c, ok, err := read()
		if err != nil {
			r.logf("nodev1: capacity: node.yaml: %v", err)
			return
		}
		if !ok {
			return
		}
		if r.capacityTotal() == c {
			return
		}
		r.SetCapacity(c)
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
