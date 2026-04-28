package main

// TODO(spec22-phase5): Discord media helpers moved to internal/gateways/discord.
// This file is a shim; symbols are re-exposed for callers in cmd/mesh.

import (
	"net/http"

	"github.com/bwmarrin/discordgo"
	dgw "projectmesh.io/mesh-cli/internal/gateways/discord"
	imedia "projectmesh.io/mesh-cli/internal/media"
)

var discordImageMIMEs = imedia.ImageMIMEs

func discordMediaDir(account string) (string, error) {
	return dgw.MediaDir(account)
}

func discordMediaPath(account, msgID string, idx int, mime string) (string, error) {
	return dgw.MediaPath(account, msgID, idx, mime)
}

func pruneDiscordMedia(account string) {
	dgw.PruneMedia(account)
}

func downloadDiscordAttachment(att *discordgo.MessageAttachment, httpClient *http.Client) ([]byte, string) {
	return dgw.DownloadAttachment(att, httpClient)
}

func writeDiscordAttachment(account, msgID string, idx int, mime string, data []byte) (string, string) {
	return dgw.WriteAttachment(account, msgID, idx, mime, data)
}
