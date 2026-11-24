package nodev1

import (
	"os"

	"gopkg.in/yaml.v3"
)

// ValidationReserve is the room a node keeps for its validation kinds
// alone (goal a-v1-node-reserves-room-for-validations-and-sizes-its-
// reserves-from-use P1): an ask of one of Kinds is admitted into it at
// once, however full the node is with builds, and no other kind may use
// it. Unlike AssistantReserve, it is never named to Mesh on a take — a
// purely local admission — and its lease keeps its job slot and shared
// build cache hold, a real build rather than a lightweight assistant
// ask. It is a setting: `mesh node run`'s --validation-reserve-* flags,
// each field overridden by node.yaml's `v1.validation_reserve`, read at
// start.
type ValidationReserve struct {
	CPUs  float64
	MemGB float64
	// Kinds are short kind names ("validate", "evaluate").
	Kinds []string
}

type nodeYAMLValidationReserve struct {
	V1 struct {
		ValidationReserve *struct {
			CPUs  *float64 `yaml:"cpus"`
			MemGB *float64 `yaml:"mem_gb"`
			Kinds []string `yaml:"kinds"`
		} `yaml:"validation_reserve"`
	} `yaml:"v1"`
}

// ReadValidationReserve is def with every field node.yaml's
// `v1.validation_reserve` sets substituted for its own; def itself when
// the file or the block is missing.
func ReadValidationReserve(path string, def ValidationReserve) (ValidationReserve, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return def, nil
		}
		return def, err
	}
	var cfg nodeYAMLValidationReserve
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return def, err
	}
	out := def
	v := cfg.V1.ValidationReserve
	if v == nil {
		return out, nil
	}
	if v.CPUs != nil {
		out.CPUs = *v.CPUs
	}
	if v.MemGB != nil {
		out.MemGB = *v.MemGB
	}
	if v.Kinds != nil {
		out.Kinds = v.Kinds
	}
	return out, nil
}
