package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Model B: the daemon does NOT supervise channel subprocesses. Claude Code
// spawns them on session start via the .claude/settings.json mcpServers
// entries written by WriteClaudeSettings. The helpers below exist only so
// `mesh inspect` can surface channel metadata and any status the subprocess
// chooses to drop on disk.

// ChannelStatus is the per-channel view surfaced via `mesh inspect`. In Model
// B the daemon can only report configured type/index plus whatever the
// channel-server has written to its status.json side-file. Running-state and
// pid are the channel-server's responsibility to publish, not ours to observe.
type ChannelStatus struct {
	Type            string         `json:"type"`
	Index           int            `json:"index"`
	Status          string         `json:"status,omitempty"`
	ChannelID       string         `json:"channelId,omitempty"`
	MessagesIn      int            `json:"messagesIn,omitempty"`
	MessagesOut     int            `json:"messagesOut,omitempty"`
	MessagesDropped map[string]int `json:"messagesDropped,omitempty"`
}

// ChannelStatusesForAgent returns one ChannelStatus per configured channel,
// merging in any status.json a channel-server has written. Channels without a
// status file come back with Status="unknown" — a running claude session will
// keep the file refreshed; missing file means the subprocess hasn't booted
// (or claude itself is stopped).
func ChannelStatusesForAgent(agentName string, channels []ChannelSpec) []ChannelStatus {
	out := make([]ChannelStatus, 0, len(channels))
	for i, ch := range channels {
		st := ChannelStatus{Type: ch.Type, Index: i, Status: "unknown"}
		if disk, ok := readChannelStatusFile(agentName, ch.Type, i); ok {
			// Preserve the on-disk Type/Index when present, but fall back to the
			// configured values if the file omitted them.
			if disk.Type != "" {
				st.Type = disk.Type
			}
			st.Status = disk.Status
			st.ChannelID = disk.ChannelID
			st.MessagesIn = disk.MessagesIn
			st.MessagesOut = disk.MessagesOut
			st.MessagesDropped = disk.MessagesDropped
		}
		out = append(out, st)
	}
	return out
}

// channelStatusFilePath is the side-channel where channel-servers publish
// their live metrics. The daemon reads it (best-effort) for inspect.
func channelStatusFilePath(agentName, chanType string, index int) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mesh", "agents", agentName, "channels",
		fmt.Sprintf("%s-%d.status.json", chanType, index)), nil
}

func readChannelStatusFile(agentName, chanType string, index int) (ChannelStatus, bool) {
	path, err := channelStatusFilePath(agentName, chanType, index)
	if err != nil {
		return ChannelStatus{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ChannelStatus{}, false
	}
	var st ChannelStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return ChannelStatus{}, false
	}
	st.Index = index
	return st, true
}

// WriteChannelStatusFile is called by channel-server subcommands to publish
// metrics. Atomic write via temp + rename so concurrent inspect reads never
// see a partial file.
func WriteChannelStatusFile(agentName, chanType string, index int, st ChannelStatus) error {
	path, err := channelStatusFilePath(agentName, chanType, index)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
