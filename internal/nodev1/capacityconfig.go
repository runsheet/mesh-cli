package nodev1

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// CapacityOverride is node.yaml's `v1.capacity`, read field by field
// for the node's own startup (ADR-044 amendment, 2026-11-03, "flags
// become defaults"): a field the block sets overrides the matching
// `mesh node run` flag; a field it leaves out leaves the flag's value
// in force. A missing file or no capacity block overrides nothing.
type CapacityOverride struct {
	CPUs  *float64
	MemGB *float64
	Slots *int
}

// ReadCapacityOverride reads node.yaml's `v1.capacity` for the
// flags-become-defaults rule, a pointer per field it sets and nil for
// one it leaves out. A missing file or no capacity block reads as a
// zero CapacityOverride, overriding nothing.
func ReadCapacityOverride(path string) (CapacityOverride, error) {
	cfg, ok, err := readCapacityYAML(path)
	if err != nil || !ok {
		return CapacityOverride{}, err
	}
	return CapacityOverride{CPUs: cfg.CPUs, MemGB: cfg.MemGB, Slots: cfg.Slots}, nil
}

// Apply is def with every field o sets substituted for def's own — the
// command's own flags default a field node.yaml leaves out, and
// node.yaml's value wins the one it sets (ADR-044 amendment,
// 2026-11-03, "flags become defaults").
func (o CapacityOverride) Apply(def meshapi.Capacity) meshapi.Capacity {
	out := def
	if o.CPUs != nil {
		out.CPUs = *o.CPUs
	}
	if o.MemGB != nil {
		out.MemGB = *o.MemGB
	}
	if o.Slots != nil {
		out.Slots = *o.Slots
	}
	return out
}

// SetCapacityConfig writes c as node.yaml's whole `v1.capacity` block
// (ADR-044 amendment, 2026-11-03, "`mesh node set` signals the running
// node"): it touches only v1.capacity.{cpus,mem_gb,slots}, by editing a
// generic YAML tree rather than through the legacy
// internal/node.Config, whose Save marshals its own typed struct and
// would silently drop a top-level `v1:` key that struct has no field
// for. Every other key of node.yaml — the legacy node's own included —
// is kept as it was; the file is created, with the one block, when it
// does not exist yet.
func SetCapacityConfig(path string, c meshapi.Capacity) error {
	doc, err := readYAMLDocument(path)
	if err != nil {
		return err
	}
	root := doc.Content[0]
	v1 := ensureMapKey(root, "v1")
	capacity := ensureMapKey(v1, "capacity")
	setMapScalar(capacity, "cpus", floatScalar(c.CPUs))
	setMapScalar(capacity, "mem_gb", floatScalar(c.MemGB))
	setMapScalar(capacity, "slots", intScalar(c.Slots))

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// readYAMLDocument reads path into a document node with a mapping
// root, building a fresh empty one when the file does not exist, is
// empty, or its top level is not a mapping — so a caller can touch one
// key of it and marshal the rest back as it was, comments included.
func readYAMLDocument(path string) (*yaml.Node, error) {
	var doc yaml.Node
	if b, err := os.ReadFile(path); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("node.yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	return &doc, nil
}

// ensureMapKey is m's value at key, a mapping: added, empty, if m has
// no such key; reset to an empty mapping if it names something else
// (null, a scalar, a sequence).
func ensureMapKey(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			if v.Kind != yaml.MappingNode {
				*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}
			return v
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, k, v)
	return v
}

// setMapScalar sets m's value at key to value, added at the end when
// m has no such key yet.
func setMapScalar(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	m.Content = append(m.Content, k, value)
}

func floatScalar(v float64) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(v, 'g', -1, 64)}
}

func intScalar(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(v)}
}
