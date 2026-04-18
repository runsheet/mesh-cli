package main

import "fmt"

// ChannelSpec is the opaque-config representation of a single channel binding
// on an agent. Type selects the channel implementation; Config is a per-type
// map the implementation interprets (e.g. Discord channelId + tokenEnv).
//
// The same shape is used in templates (AgentYAMLSpec.Channels), the resolved
// Template, and the persisted AgentDef — so parsing, persistence, and
// materialization share one vocabulary.
type ChannelSpec struct {
	Type   string         `json:"type" yaml:"type"`
	Config map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
}

// channelTypeRegistry is the slice-1 allow-list. Slice 2 adds "discord".
// Keeping this in-tree avoids an init-time registration dance and makes the
// allowed set grep-able.
var channelTypeRegistry = map[string]bool{
	"echo": true,
}

// validateChannelSpecs checks that each channel in the slice has a known type
// and a non-empty type string. Returns the first error encountered so the user
// fixes them one at a time. Called at template-parse time.
func validateChannelSpecs(specs []ChannelSpec) error {
	for i, c := range specs {
		if c.Type == "" {
			return fmt.Errorf("channels[%d]: type is required", i)
		}
		if !channelTypeRegistry[c.Type] {
			return fmt.Errorf("channels[%d]: unknown type %q (known: echo)", i, c.Type)
		}
	}
	return nil
}
