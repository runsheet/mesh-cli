package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// ipcVersion is the protocol version embedded in every envelope. A gateway
// or shim that receives a version it doesn't recognise responds with an error
// envelope and closes the connection.
const ipcVersion = 1

// ipcChannelOverride is the wire representation of a per-channel override
// within a guild-mode subscribe envelope. Mirrors discordChannelOverride but
// uses plain bool/string (not pointers) — unset fields are omitted via
// omitempty so the gateway can tell "override present" from "override absent".
type ipcChannelOverride struct {
	ChannelID      string   `json:"channelId"`
	RequireMention *bool    `json:"requireMention,omitempty"`
	InboundPrefix  *string  `json:"inboundPrefix,omitempty"`
	AllowedUsers   []string `json:"allowedUsers,omitempty"`
	AllowedRoles   []string `json:"allowedRoles,omitempty"`
}

// ipcEnvelope is the common wire shape for all shim↔gateway messages over the
// gateway Unix socket. Line-delimited JSON; one envelope per line.
//
// Shim → gateway types: subscribe, unsubscribe, send
// Gateway → shim types: subscribe-ack, inbound, error
type ipcEnvelope struct {
	V    int    `json:"v"`
	Type string `json:"type"`

	// subscribe fields (shim → gateway)
	Agent            string               `json:"agent,omitempty"`
	Index            int                  `json:"index,omitempty"`
	RoutingKey       string               `json:"routingKey,omitempty"`
	SubscriptionMode string               `json:"subscriptionMode,omitempty"` // "channel" (default) or "guild"
	AllowedUsers     []string             `json:"allowedUsers,omitempty"`
	AllowedRoles     []string             `json:"allowedRoles,omitempty"`
	RequireMention   bool                 `json:"requireMention,omitempty"`
	InboundPrefix    string               `json:"inboundPrefix,omitempty"`
	ChannelOverrides []ipcChannelOverride `json:"channelOverrides,omitempty"` // guild mode only

	// send fields (shim → gateway)
	Content   string `json:"content,omitempty"`
	ChannelID string `json:"channelId,omitempty"` // send: optional explicit target (guild mode); inbound: originating channel

	// inbound fields (gateway → shim)
	Sender    string `json:"sender,omitempty"`    // display name (e.g. Discord username)
	MessageID string `json:"messageId,omitempty"` // platform-specific message id
	UserID    string `json:"userId,omitempty"`    // platform-specific sender user id
	Timestamp string `json:"ts,omitempty"`        // ISO-8601 message timestamp

	// error fields (gateway → shim)
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ipcWriter wraps a net.Conn and serialises ipcEnvelope values as
// newline-terminated JSON. Safe for single-goroutine use only; callers must
// coordinate their own locking when writing from multiple goroutines.
type ipcWriter struct {
	w io.Writer
}

func newIPCWriter(w io.Writer) *ipcWriter { return &ipcWriter{w: w} }

func (iw *ipcWriter) Write(env ipcEnvelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("ipc marshal: %w", err)
	}
	data = append(data, '\n')
	_, err = iw.w.Write(data)
	return err
}

// ipcReader wraps a buffered reader and yields one ipcEnvelope per call.
// Returns io.EOF when the underlying connection closes cleanly.
type ipcReader struct {
	s *bufio.Scanner
}

func newIPCReader(r io.Reader) *ipcReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	return &ipcReader{s: sc}
}

func (ir *ipcReader) Read() (ipcEnvelope, error) {
	if !ir.s.Scan() {
		if err := ir.s.Err(); err != nil {
			return ipcEnvelope{}, err
		}
		return ipcEnvelope{}, io.EOF
	}
	var env ipcEnvelope
	if err := json.Unmarshal(ir.s.Bytes(), &env); err != nil {
		return ipcEnvelope{}, fmt.Errorf("ipc unmarshal: %w", err)
	}
	if env.V != ipcVersion {
		return ipcEnvelope{}, fmt.Errorf("ipc protocol version mismatch: got %d want %d", env.V, ipcVersion)
	}
	return env, nil
}

// dialGateway opens a Unix-socket connection to the gateway socket and returns
// a reader/writer pair. The caller is responsible for closing the conn.
func dialGateway(sockPath string) (net.Conn, *ipcReader, *ipcWriter, error) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dial gateway %s: %w", sockPath, err)
	}
	return conn, newIPCReader(conn), newIPCWriter(conn), nil
}

// ipcSubscribeChannelEnv builds the subscribe envelope for channel-mode
// (subscriptionMode defaults to "channel" so the field is omitted for
// backward-compat with old gateways).
func ipcSubscribeChannelEnv(agent string, index int, channelID, inboundPrefix string, allowedUsers, allowedRoles []string, requireMention bool) ipcEnvelope {
	return ipcEnvelope{
		V:              ipcVersion,
		Type:           "subscribe",
		Agent:          agent,
		Index:          index,
		RoutingKey:     channelID,
		InboundPrefix:  inboundPrefix,
		AllowedUsers:   allowedUsers,
		AllowedRoles:   allowedRoles,
		RequireMention: requireMention,
	}
}

// ipcSubscribeGuildEnv builds the subscribe envelope for guild-mode.
func ipcSubscribeGuildEnv(agent string, index int, guildID, inboundPrefix string, allowedUsers, allowedRoles []string, requireMention bool, overrides []ipcChannelOverride) ipcEnvelope {
	return ipcEnvelope{
		V:                ipcVersion,
		Type:             "subscribe",
		Agent:            agent,
		Index:            index,
		SubscriptionMode: "guild",
		RoutingKey:       guildID,
		InboundPrefix:    inboundPrefix,
		AllowedUsers:     allowedUsers,
		AllowedRoles:     allowedRoles,
		RequireMention:   requireMention,
		ChannelOverrides: overrides,
	}
}

// ipcSubscribeEnv is the original helper retained for backward-compat with
// call sites that predate guild mode. It always produces a channel-mode
// envelope (subscriptionMode omitted).
func ipcSubscribeEnv(agent string, index int, routingKey, inboundPrefix string, allowedUsers, allowedRoles []string, requireMention bool) ipcEnvelope {
	return ipcSubscribeChannelEnv(agent, index, routingKey, inboundPrefix, allowedUsers, allowedRoles, requireMention)
}

// ipcSendEnv builds the send envelope a shim forwards to the gateway.
// channelID is the explicit destination channel; for channel mode it equals
// routingKey; for guild mode it may differ from the guildId routingKey.
func ipcSendEnv(channelID, content string) ipcEnvelope {
	return ipcEnvelope{V: ipcVersion, Type: "send", ChannelID: channelID, Content: content}
}

// ipcUnsubscribeEnv builds the unsubscribe envelope.
func ipcUnsubscribeEnv(routingKey string) ipcEnvelope {
	return ipcEnvelope{V: ipcVersion, Type: "unsubscribe", RoutingKey: routingKey}
}

// ipcAckEnv builds the subscribe-ack the gateway sends back.
func ipcAckEnv(routingKey string) ipcEnvelope {
	return ipcEnvelope{V: ipcVersion, Type: "subscribe-ack", RoutingKey: routingKey}
}

// ipcInboundEnvFull builds the inbound envelope with all metadata fields
// populated. Used by the discord gateway so the shim can emit Claude Code's
// `notifications/claude/channel` with a proper meta block.
func ipcInboundEnvFull(routingKey, channelID, sender, messageID, userID, timestamp, content string) ipcEnvelope {
	return ipcEnvelope{
		V:          ipcVersion,
		Type:       "inbound",
		RoutingKey: routingKey,
		ChannelID:  channelID,
		Sender:     sender,
		MessageID:  messageID,
		UserID:     userID,
		Timestamp:  timestamp,
		Content:    content,
	}
}

// ipcInboundEnv builds the inbound envelope the gateway sends to a shim.
// channelID is the originating Discord channel (important for guild-mode shims
// that need to know which channel to reply to).
func ipcInboundEnv(routingKey, channelID, sender, content string) ipcEnvelope {
	return ipcEnvelope{V: ipcVersion, Type: "inbound", RoutingKey: routingKey, ChannelID: channelID, Sender: sender, Content: content}
}

// ipcErrorEnv builds an error envelope.
func ipcErrorEnv(reason, detail string) ipcEnvelope {
	return ipcEnvelope{V: ipcVersion, Type: "error", Reason: reason, Detail: detail}
}
