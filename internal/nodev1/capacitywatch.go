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
// never meshapi.Capacity's json ones, which yaml.v3 does not read.
type nodeYAMLCapacity struct {
	V1 struct {
		Capacity *struct {
			CPUs  float64 `yaml:"cpus"`
			MemGB float64 `yaml:"mem_gb"`
			Slots int     `yaml:"slots"`
		} `yaml:"capacity"`
	} `yaml:"v1"`
}

// ReadCapacityConfig reads node.yaml's `v1.capacity`: false with no
// error when the file does not exist or names no capacity.
func ReadCapacityConfig(path string) (meshapi.Capacity, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return meshapi.Capacity{}, false, nil
		}
		return meshapi.Capacity{}, false, err
	}
	var cfg nodeYAMLCapacity
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return meshapi.Capacity{}, false, err
	}
	if cfg.V1.Capacity == nil {
		return meshapi.Capacity{}, false, nil
	}
	return meshapi.Capacity{CPUs: cfg.V1.Capacity.CPUs, MemGB: cfg.V1.Capacity.MemGB, Slots: cfg.V1.Capacity.Slots}, true, nil
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
