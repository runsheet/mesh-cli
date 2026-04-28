package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// This file contains Discord helpers shared between the gateway and the shim.
// The v0 per-agent `channel-server discord` command (tokenEnv model) has been
// removed per spec 12 decisions — no compat shim.

// sendDiscordMessage posts content to channelID, splitting into ≤2000-char
// parts on paragraph/sentence boundaries. Returns the ids of every message
// actually sent so the caller can surface them.
//
// metrics may be nil (gateway passes nil; v0 shim metrics are gone).
func sendDiscordMessage(
	session *discordgo.Session,
	channelID, content string,
	_ any, // kept for call-site compatibility during refactor
	logf func(string, ...any),
) ([]string, error) {
	if session == nil {
		return nil, fmt.Errorf("discord session not open")
	}
	parts := splitDiscordContent(content, 2000)
	ids := make([]string, 0, len(parts))
	for i, p := range parts {
		toSend := p
		if len(parts) > 1 {
			toSend = fmt.Sprintf("(%d/%d) %s", i+1, len(parts), p)
			if len(toSend) > 2000 {
				toSend = toSend[:2000]
			}
		}
		msg, err := session.ChannelMessageSend(channelID, toSend)
		if err != nil {
			return ids, fmt.Errorf("send part %d/%d: %w", i+1, len(parts), err)
		}
		ids = append(ids, msg.ID)
		if logf != nil {
			logf("sent part %d/%d id=%s len=%d", i+1, len(parts), msg.ID, len(toSend))
		}
		if i < len(parts)-1 {
			time.Sleep(250 * time.Millisecond)
		}
	}
	return ids, nil
}

// sendDiscordMessageWithAttachment posts an image file to channelID as a
// Discord file attachment with content as the message text (caption).
// content may be empty; Discord renders the image without a caption.
// Returns a single-element message-ID slice on success.
func sendDiscordMessageWithAttachment(
	session *discordgo.Session,
	channelID, content string,
	att ipcAttachment,
	logf func(string, ...any),
) ([]string, error) {
	if session == nil {
		return nil, fmt.Errorf("discord session not open")
	}
	f, err := os.Open(att.Path)
	if err != nil {
		return nil, fmt.Errorf("open attachment: %w", err)
	}
	defer f.Close()

	msg, err := session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: content,
		Files: []*discordgo.File{{
			Name:        filepath.Base(att.Path),
			ContentType: att.MIMEType,
			Reader:      f,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("discord send with attachment: %w", err)
	}
	if logf != nil {
		logf("sent attachment id=%s channelId=%s mime=%s", msg.ID, channelID, att.MIMEType)
	}
	return []string{msg.ID}, nil
}

// splitDiscordContent splits content into chunks ≤ limit bytes, preferring
// blank-line, single-newline, and sentence boundaries before a hard byte cut.
// We leave room for a `(i/n) ` prefix up to 10 chars.
func splitDiscordContent(content string, limit int) []string {
	const prefixReserve = 10
	effective := limit - prefixReserve
	if len(content) <= limit {
		return []string{content}
	}

	var out []string
	remaining := content
	for len(remaining) > effective {
		cut := findBoundary(remaining, effective)
		if cut <= 0 {
			cut = effective
		}
		out = append(out, strings.TrimRight(remaining[:cut], " \t\n"))
		remaining = strings.TrimLeft(remaining[cut:], " \t\n")
	}
	if len(remaining) > 0 {
		out = append(out, remaining)
	}
	return out
}

// findBoundary locates the best break point in s[:max].
func findBoundary(s string, max int) int {
	if len(s) <= max {
		return len(s)
	}
	window := s[:max]
	if idx := strings.LastIndex(window, "\n\n"); idx > 0 {
		return idx + 2
	}
	if idx := strings.LastIndex(window, "\n"); idx > 0 {
		return idx + 1
	}
	for _, sep := range []string{". ", "! ", "? "} {
		if idx := strings.LastIndex(window, sep); idx > 0 {
			return idx + len(sep)
		}
	}
	if idx := strings.LastIndex(window, " "); idx > 0 {
		return idx + 1
	}
	return -1
}

// contains reports whether needle is in haystack. Used by the gateway's
// allow-list filter.
func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}
