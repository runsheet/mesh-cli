package nodev1

import (
	"os"

	"gopkg.in/yaml.v3"
)

// AssistantReserve is the room a node keeps for its assistant kinds
// alone (goal one-assistant-dispatcher-per-org-answers-every-ask P1): an
// ask of one of Kinds is admitted into it at once, however full the
// node is with builds and validations, and no other kind may use it.
// It is a setting: `mesh node run`'s --assistant-reserve-* flags, each
// field overridden by node.yaml's `v1.assistant_reserve`, read at start.
type AssistantReserve struct {
	CPUs  float64
	MemGB float64
	// Kinds are short kind names ("goal-chat").
	Kinds []string
}

type nodeYAMLAssistantReserve struct {
	V1 struct {
		AssistantReserve *struct {
			CPUs  *float64 `yaml:"cpus"`
			MemGB *float64 `yaml:"mem_gb"`
			Kinds []string `yaml:"kinds"`
		} `yaml:"assistant_reserve"`
	} `yaml:"v1"`
}

// ReadAssistantReserve is def with every field node.yaml's
// `v1.assistant_reserve` sets substituted for its own; def itself when
// the file or the block is missing.
func ReadAssistantReserve(path string, def AssistantReserve) (AssistantReserve, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return def, nil
		}
		return def, err
	}
	var cfg nodeYAMLAssistantReserve
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return def, err
	}
	a := cfg.V1.AssistantReserve
	if a == nil {
		return def, nil
	}
	out := def
	if a.CPUs != nil {
		out.CPUs = *a.CPUs
	}
	if a.MemGB != nil {
		out.MemGB = *a.MemGB
	}
	if a.Kinds != nil {
		out.Kinds = a.Kinds
	}
	return out, nil
}
