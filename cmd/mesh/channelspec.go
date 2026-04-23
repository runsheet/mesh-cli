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

// channelTypeRegistry is the allow-list of known channel types. Keeping this
// in-tree avoids an init-time registration dance and makes the allowed set
// grep-able.
var channelTypeRegistry = map[string]bool{
	"echo":    true,
	"discord": true,
}

// discordChannelOverride is a per-channel override within a guild-mode
// subscription. channelId is required; all other fields are optional and
// merged on top of the guild defaults at runtime.
type discordChannelOverride struct {
	ChannelID      string   `json:"channelId"`
	RequireMention *bool    `json:"requireMention,omitempty"`
	InboundPrefix  *string  `json:"inboundPrefix,omitempty"`
	AllowedUsers   []string `json:"allowedUsers,omitempty"`
	AllowedRoles   []string `json:"allowedRoles,omitempty"`
}

// discordChannelConfig is the structured representation of a discord channel
// config block. Used internally after parsing from the opaque map.
type discordChannelConfig struct {
	Account          string
	SubscriptionMode string // "channel" (default) or "guild"
	ChannelID        string // channel mode
	GuildID          string // guild mode
	RequireMention   bool
	AllowedUsers     []string
	AllowedRoles     []string
	InboundPrefix    string
	ChannelOverrides []discordChannelOverride // guild mode only
}

// validateChannelSpecs checks that each channel in the slice has a known type
// and a populated per-type config. Returns the first error encountered so the
// user fixes them one at a time. Called at template-parse time.
//
// Also enforces the within-agent uniqueness rule: no two channel entries (or
// override entries within guild-mode entries) may claim the same channelId.
func validateChannelSpecs(specs []ChannelSpec) error {
	// claimedChannelIDs tracks channelIds claimed within this agent for
	// within-agent duplicate detection.
	claimedChannelIDs := map[string]string{} // channelId → "channels[i]" description

	for i, c := range specs {
		if c.Type == "" {
			return fmt.Errorf("channels[%d]: type is required", i)
		}
		if !channelTypeRegistry[c.Type] {
			return fmt.Errorf("channels[%d]: unknown type %q (known: echo, discord)", i, c.Type)
		}
		switch c.Type {
		case "discord":
			if err := validateDiscordConfig(c.Config, i, claimedChannelIDs); err != nil {
				return fmt.Errorf("channels[%d] (discord): %w", i, err)
			}
		}
	}
	return nil
}

// validateDiscordConfig enforces the config shape rules from spec 12 §3 plus
// the guild-mode extensions. claimedChannelIDs is updated in place for
// within-agent duplicate detection; pass nil to skip that check.
func validateDiscordConfig(cfg map[string]any, idx int, claimedChannelIDs map[string]string) error {
	acc, _ := cfg["account"].(string)
	if acc == "" {
		return fmt.Errorf("config.account is required (reference a paired discord account by name)")
	}

	mode, _ := cfg["subscriptionMode"].(string)
	if mode == "" {
		mode = "channel"
	}
	if mode != "channel" && mode != "guild" {
		return fmt.Errorf("config.subscriptionMode must be \"channel\" or \"guild\", got %q", mode)
	}

	desc := fmt.Sprintf("channels[%d]", idx)

	switch mode {
	case "channel":
		if _, hasGuild := cfg["guildId"]; hasGuild {
			return fmt.Errorf("config.guildId is not allowed in channel mode")
		}
		if _, hasOverrides := cfg["channelOverrides"]; hasOverrides {
			return fmt.Errorf("config.channelOverrides is not allowed in channel mode")
		}
		ch, _ := cfg["channelId"].(string)
		if ch == "" {
			return fmt.Errorf("config.channelId is required in channel mode")
		}
		if claimedChannelIDs != nil {
			if prior, conflict := claimedChannelIDs[ch]; conflict {
				return fmt.Errorf("config.channelId %q already claimed by %s within this agent", ch, prior)
			}
			claimedChannelIDs[ch] = desc
		}

	case "guild":
		if _, hasCh := cfg["channelId"]; hasCh {
			return fmt.Errorf("config.channelId is not allowed in guild mode (set guildId instead)")
		}
		guildID, _ := cfg["guildId"].(string)
		if guildID == "" {
			return fmt.Errorf("config.guildId is required in guild mode")
		}
		overrides, err := parseDiscordChannelOverrides(cfg["channelOverrides"])
		if err != nil {
			return fmt.Errorf("config.channelOverrides: %w", err)
		}
		for j, ov := range overrides {
			if ov.ChannelID == "" {
				return fmt.Errorf("config.channelOverrides[%d].channelId is required", j)
			}
			if claimedChannelIDs != nil {
				ovDesc := fmt.Sprintf("%s.channelOverrides[%d]", desc, j)
				if prior, conflict := claimedChannelIDs[ov.ChannelID]; conflict {
					return fmt.Errorf("config.channelOverrides[%d].channelId %q already claimed by %s within this agent", j, ov.ChannelID, prior)
				}
				claimedChannelIDs[ov.ChannelID] = ovDesc
			}
		}
	}

	return nil
}

// parseDiscordChannelOverrides converts the raw YAML/JSON value of
// channelOverrides into a typed slice. Accepts nil (no overrides) or a
// []any of map[string]any entries.
func parseDiscordChannelOverrides(raw any) ([]discordChannelOverride, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("expected array, got %T", raw)
	}
	out := make([]discordChannelOverride, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("[%d]: expected object, got %T", i, item)
		}
		var ov discordChannelOverride
		ov.ChannelID, _ = m["channelId"].(string)
		if req, ok := m["requireMention"]; ok {
			if b, ok := req.(bool); ok {
				ov.RequireMention = &b
			}
		}
		if pfx, ok := m["inboundPrefix"]; ok {
			if s, ok := pfx.(string); ok {
				ov.InboundPrefix = &s
			}
		}
		ov.AllowedUsers = anyToStringSlice(m["allowedUsers"])
		ov.AllowedRoles = anyToStringSlice(m["allowedRoles"])
		out = append(out, ov)
	}
	return out, nil
}

// anyToStringSlice converts YAML/JSON list representations to []string.
func anyToStringSlice(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, it := range x {
			if s, ok := it.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// parseDiscordChannelConfig builds a discordChannelConfig from the opaque
// config map stored in ChannelSpec.Config. Caller should have already
// validated the map; errors here indicate internal inconsistency.
func parseDiscordChannelConfig(cfg map[string]any) discordChannelConfig {
	mode, _ := cfg["subscriptionMode"].(string)
	if mode == "" {
		mode = "channel"
	}
	out := discordChannelConfig{
		Account:          stringFromMap(cfg, "account"),
		SubscriptionMode: mode,
		ChannelID:        stringFromMap(cfg, "channelId"),
		GuildID:          stringFromMap(cfg, "guildId"),
		RequireMention:   boolFromMap(cfg, "requireMention"),
		InboundPrefix:    stringFromMap(cfg, "inboundPrefix"),
		AllowedUsers:     anyToStringSlice(cfg["allowedUsers"]),
		AllowedRoles:     anyToStringSlice(cfg["allowedRoles"]),
	}
	overrides, _ := parseDiscordChannelOverrides(cfg["channelOverrides"])
	out.ChannelOverrides = overrides
	return out
}

func stringFromMap(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func boolFromMap(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}
