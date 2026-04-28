// Package ipc defines the wire protocol used between the daemon router,
// gateways, and the mesh-shim. All communication is line-delimited JSON over
// Unix sockets; one Envelope per line.
package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// Version is the protocol version embedded in every Envelope. A gateway or
// shim that receives a version it doesn't recognise responds with an error
// envelope and closes the connection.
const Version = 1

// ChannelOverride is the wire representation of a per-channel override within
// a guild-mode subscribe envelope. Mirrors discordChannelOverride in
// channelspec.go but uses plain bool/string (not pointers) — unset fields are
// omitted via omitempty so the gateway can tell "override present" from
// "override absent".
type ChannelOverride struct {
	ChannelID      string   `json:"channelId"`
	RequireMention *bool    `json:"requireMention,omitempty"`
	InboundPrefix  *string  `json:"inboundPrefix,omitempty"`
	AllowedUsers   []string `json:"allowedUsers,omitempty"`
	AllowedRoles   []string `json:"allowedRoles,omitempty"`
}

// Envelope is the common wire shape for all shim↔gateway messages over the
// gateway Unix socket. Line-delimited JSON; one Envelope per line.
//
// Shim → gateway types: subscribe, unsubscribe, send
// Gateway → shim types: subscribe-ack, inbound, error
// Router → shim types: subscribe-ack, inbound, error (same as gateway)
// Shim → router types: subscribe, unsubscribe, send (same as shim→gateway)
type Envelope struct {
	V    int    `json:"v"`
	Type string `json:"type"`

	// subscribe fields (shim → gateway / router)
	Agent            string            `json:"agent,omitempty"`
	Index            int               `json:"index,omitempty"`
	RoutingKey       string            `json:"routingKey,omitempty"`
	SubscriptionMode string            `json:"subscriptionMode,omitempty"` // "channel" (default) or "guild"
	AllowedUsers     []string          `json:"allowedUsers,omitempty"`
	AllowedRoles     []string          `json:"allowedRoles,omitempty"`
	RequireMention   bool              `json:"requireMention,omitempty"`
	InboundPrefix    string            `json:"inboundPrefix,omitempty"`
	ChannelOverrides []ChannelOverride `json:"channelOverrides,omitempty"` // guild mode only

	// send fields (shim → gateway / router)
	Content   string `json:"content,omitempty"`
	ChannelID string `json:"channelId,omitempty"` // send: optional explicit target (guild mode); inbound: originating channel

	// Source identifies the backend platform an inbound envelope originated
	// from. Set by the daemon router when it fans out from a gateway to the
	// mesh-shim; absent on legacy direct-gateway paths. Values: "discord",
	// "whatsapp", "peers".
	Source string `json:"source,omitempty"`

	// Target is the URL-shape destination the mesh-shim supplies on outbound
	// sends (e.g. "discord://channel/<id>", "discord://dm/<userId>"). The
	// daemon router parses the scheme and forwards to the right gateway.
	// Absent on legacy direct-gateway paths.
	Target string `json:"target,omitempty"`

	// inbound fields (gateway → shim / router)
	Sender    string `json:"sender,omitempty"`    // display name (e.g. Discord username)
	MessageID string `json:"messageId,omitempty"` // platform-specific message id
	UserID    string `json:"userId,omitempty"`    // platform-specific sender user id
	Timestamp string `json:"ts,omitempty"`        // ISO-8601 message timestamp

	// peers inbound fields (broker → router → shim). PeerID is the sending
	// agent name; Team is set only for team-scoped sends.
	PeerID string `json:"peerId,omitempty"`
	Team   string `json:"team,omitempty"`

	// error fields (gateway → shim)
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`

	// Attachments carries downloaded media files attached to an inbound message.
	// Path is empty and FailReason is set on download failure. omitempty keeps
	// text-only envelopes wire-identical to pre-v1-media envelopes (no version bump).
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment describes a single downloaded media file attached to an inbound
// message. Path is empty and FailReason is set on download failure.
type Attachment struct {
	Type       string `json:"type"`                 // "image" (v1); future: "audio","video","document"
	Path       string `json:"path,omitempty"`       // absolute local path; empty on failure
	MIMEType   string `json:"mimeType,omitempty"`   // MIME as reported by whatsmeow
	FailReason string `json:"failReason,omitempty"` // non-empty on failure
}

// Writer wraps an io.Writer and serialises Envelope values as
// newline-terminated JSON. Safe for single-goroutine use only; callers must
// coordinate their own locking when writing from multiple goroutines.
type Writer struct {
	w io.Writer
}

// NewWriter returns a Writer that writes to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write serialises env as a newline-terminated JSON line.
func (iw *Writer) Write(env Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("ipc marshal: %w", err)
	}
	data = append(data, '\n')
	_, err = iw.w.Write(data)
	return err
}

// Reader wraps a buffered reader and yields one Envelope per call.
// Returns io.EOF when the underlying connection closes cleanly.
type Reader struct {
	s *bufio.Scanner
}

// NewReader returns a Reader that reads from r.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	return &Reader{s: sc}
}

// Read returns the next Envelope from the stream.
func (ir *Reader) Read() (Envelope, error) {
	if !ir.s.Scan() {
		if err := ir.s.Err(); err != nil {
			return Envelope{}, err
		}
		return Envelope{}, io.EOF
	}
	var env Envelope
	if err := json.Unmarshal(ir.s.Bytes(), &env); err != nil {
		return Envelope{}, fmt.Errorf("ipc unmarshal: %w", err)
	}
	if env.V != Version {
		return Envelope{}, fmt.Errorf("ipc protocol version mismatch: got %d want %d", env.V, Version)
	}
	return env, nil
}

// DialGateway opens a Unix-socket connection to the gateway socket and returns
// a reader/writer pair. The caller is responsible for closing the conn.
func DialGateway(sockPath string) (net.Conn, *Reader, *Writer, error) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dial gateway %s: %w", sockPath, err)
	}
	return conn, NewReader(conn), NewWriter(conn), nil
}

// PeerEntryWire is the JSON shape for one peer in a peers-list-resp envelope.
// Content on the envelope carries a JSON-encoded []PeerEntryWire.
type PeerEntryWire struct {
	PeerID     string   `json:"peer_id"`
	NodeID     string   `json:"node_id"`
	Teams      []string `json:"teams"`
	LastSeenAt string   `json:"last_seen_at"` // RFC3339
	Summary    string   `json:"summary,omitempty"`
}

// SubscribeChannelEnv builds the subscribe envelope for channel-mode
// (subscriptionMode defaults to "channel" so the field is omitted for
// backward-compat with old gateways).
func SubscribeChannelEnv(agent string, index int, channelID, inboundPrefix string, allowedUsers, allowedRoles []string, requireMention bool) Envelope {
	return Envelope{
		V:              Version,
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

// SubscribeGuildEnv builds the subscribe envelope for guild-mode.
func SubscribeGuildEnv(agent string, index int, guildID, inboundPrefix string, allowedUsers, allowedRoles []string, requireMention bool, overrides []ChannelOverride) Envelope {
	return Envelope{
		V:                Version,
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

// SubscribeEnv is the original helper retained for backward-compat with call
// sites that predate guild mode. It always produces a channel-mode envelope
// (subscriptionMode omitted).
func SubscribeEnv(agent string, index int, routingKey, inboundPrefix string, allowedUsers, allowedRoles []string, requireMention bool) Envelope {
	return SubscribeChannelEnv(agent, index, routingKey, inboundPrefix, allowedUsers, allowedRoles, requireMention)
}

// SendEnv builds the send envelope a shim forwards to the gateway. channelID
// is the explicit destination channel; for channel mode it equals routingKey;
// for guild mode it may differ from the guildId routingKey.
func SendEnv(channelID, content string) Envelope {
	return Envelope{V: Version, Type: "send", ChannelID: channelID, Content: content}
}

// SendEnvWithAttachments builds a send envelope that carries media
// attachments. attachments may be nil; omitempty keeps wire format
// identical to SendEnv for text-only sends.
func SendEnvWithAttachments(target, channelID, content string, attachments []Attachment) Envelope {
	return Envelope{
		V:           Version,
		Type:        "send",
		Target:      target,
		ChannelID:   channelID,
		Content:     content,
		Attachments: attachments,
	}
}

// UnsubscribeEnv builds the unsubscribe envelope.
func UnsubscribeEnv(routingKey string) Envelope {
	return Envelope{V: Version, Type: "unsubscribe", RoutingKey: routingKey}
}

// AckEnv builds the subscribe-ack the gateway sends back.
func AckEnv(routingKey string) Envelope {
	return Envelope{V: Version, Type: "subscribe-ack", RoutingKey: routingKey}
}

// InboundEnvFull builds the inbound envelope with all metadata fields
// populated. Used by the discord and whatsapp gateways so the shim can emit
// Claude Code's `notifications/claude/channel` with a proper meta block.
// attachments may be nil for text-only messages; omitempty keeps the wire
// format unchanged for existing text-only senders.
func InboundEnvFull(routingKey, channelID, sender, messageID, userID, timestamp, content string, attachments []Attachment) Envelope {
	return Envelope{
		V:           Version,
		Type:        "inbound",
		RoutingKey:  routingKey,
		ChannelID:   channelID,
		Sender:      sender,
		MessageID:   messageID,
		UserID:      userID,
		Timestamp:   timestamp,
		Content:     content,
		Attachments: attachments,
	}
}

// InboundEnv builds the inbound envelope the gateway sends to a shim.
// channelID is the originating Discord channel (important for guild-mode shims
// that need to know which channel to reply to).
func InboundEnv(routingKey, channelID, sender, content string) Envelope {
	return Envelope{V: Version, Type: "inbound", RoutingKey: routingKey, ChannelID: channelID, Sender: sender, Content: content}
}

// ErrorEnv builds an error envelope.
func ErrorEnv(reason, detail string) Envelope {
	return Envelope{V: Version, Type: "error", Reason: reason, Detail: detail}
}

// ListGroupsEnv builds the list-groups request sent by the CLI to the gateway.
func ListGroupsEnv() Envelope {
	return Envelope{V: Version, Type: "list-groups"}
}

// ListGroupsRespEnv builds the list-groups-resp the gateway sends back.
// content is a JSON-encoded []whatsappGroupEntry.
func ListGroupsRespEnv(content string) Envelope {
	return Envelope{V: Version, Type: "list-groups-resp", Content: content}
}

// JoinGroupEnv builds the join-group request. routingKey carries the raw
// invite code (prefix already stripped by the CLI).
func JoinGroupEnv(code string) Envelope {
	return Envelope{V: Version, Type: "join-group", RoutingKey: code}
}

// JoinGroupRespEnv builds the join-group-resp the gateway sends back.
// routingKey is the resulting group JID; content is the group subject.
func JoinGroupRespEnv(jid, subject string) Envelope {
	return Envelope{V: Version, Type: "join-group-resp", RoutingKey: jid, Content: subject}
}

// PeersListEnv builds the peers-list request the shim sends to the router.
// team is optional; empty string means "all peers".
func PeersListEnv(team string) Envelope {
	return Envelope{V: Version, Type: "peers-list", RoutingKey: team}
}

// PeersListRespEnv builds the peers-list-resp the router sends back.
// content is a JSON-encoded []PeerEntryWire.
func PeersListRespEnv(content string) Envelope {
	return Envelope{V: Version, Type: "peers-list-resp", Content: content}
}

// PeersSetSummaryEnv builds the peers-set-summary request. content carries
// the summary string; the router identifies the agent from the agentRouter
// that owns the connection.
func PeersSetSummaryEnv(summary string) Envelope {
	return Envelope{V: Version, Type: "peers-set-summary", Content: summary}
}

// PeersSetSummaryRespEnv builds the peers-set-summary-resp. On success Reason
// is empty. On failure Reason carries the error string.
func PeersSetSummaryRespEnv(errReason string) Envelope {
	return Envelope{V: Version, Type: "peers-set-summary-resp", Reason: errReason}
}
