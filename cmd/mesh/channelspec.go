package main

// This file is a thin compatibility shim. All channel-spec types and validators
// now live in internal/channels; the aliases and wrappers below let the rest of
// cmd/mesh/ continue to use the old unexported names without change until the
// final callers are rewired directly to internal/channels in a later phase.

import "projectmesh.io/mesh-cli/internal/channels"

// ChannelSpec is exported because it appears in the AgentDef JSON and is
// referenced by the channel router. Use channels.Spec in new code.
type ChannelSpec = channels.Spec

type peersChannelConfig = channels.PeersChannelConfig
type discordChannelConfig = channels.DiscordChannelConfig
type discordChannelOverride = channels.DiscordChannelOverride
type whatsappChannelConfig = channels.WhatsappChannelConfig

func validateChannelSpecs(specs []ChannelSpec) error {
	return channels.ValidateSpecs(specs)
}

func parsePeersChannelConfig(cfg map[string]any) peersChannelConfig {
	return channels.ParsePeersChannelConfig(cfg)
}

func parseDiscordChannelConfig(cfg map[string]any) discordChannelConfig {
	return channels.ParseDiscordChannelConfig(cfg)
}

func parseDiscordChannelOverrides(raw any) ([]discordChannelOverride, error) {
	return channels.ParseDiscordChannelOverrides(raw)
}

func parseWhatsappChannelConfig(cfg map[string]any) whatsappChannelConfig {
	return channels.ParseWhatsappChannelConfig(cfg)
}

func isValidWhatsappJID(jid string) bool {
	return channels.IsValidWhatsappJID(jid)
}

func anyToStringSlice(v any) []string {
	return channels.AnyToStringSlice(v)
}

func stringFromMap(m map[string]any, key string) string {
	return channels.StringFromMap(m, key)
}

func boolFromMap(m map[string]any, key string) bool {
	return channels.BoolFromMap(m, key)
}
