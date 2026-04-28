package main

// TODO(spec22-phase5): Discord send helpers moved to internal/gateways/discord.
// This file is a shim; symbols are re-exposed for callers in cmd/mesh.

import (
	"github.com/bwmarrin/discordgo"
	dgw "projectmesh.io/mesh-cli/internal/gateways/discord"
)

func sendDiscordMessage(
	session *discordgo.Session,
	channelID, content string,
	metrics any,
	logf func(string, ...any),
) ([]string, error) {
	return dgw.SendMessage(session, channelID, content, metrics, logf)
}

func sendDiscordMessageWithAttachment(
	session *discordgo.Session,
	channelID, content string,
	att ipcAttachment,
	logf func(string, ...any),
) ([]string, error) {
	return dgw.SendMessageWithAttachment(session, channelID, content, att, logf)
}

func contains(haystack []string, needle string) bool {
	return dgw.Contains(haystack, needle)
}
