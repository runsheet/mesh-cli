// Package ipc defines the wire protocol used between the daemon router and
// the mesh-shim. All communication is line-delimited JSON over Unix sockets;
// one Envelope per line.
//
// Plan 00-05 removed the channel-routing supervisor (and its Discord /
// WhatsApp gateway subprocesses) along with the entire internal/channels/
// package. Only the peers-path consumers of this protocol survive. The
// guild-mode / channel-mode subscribe helpers and group-listing envelopes
// remain as dead code intentionally — they are part of the on-disk wire
// shape and cheap to leave around until the Phase 2 dispatcher rebuild
// either reuses them or a follow-up cut deletes them.
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
// a guild-mode subscribe envelope. Predates Plan 00-05's channel-routing cut;
// retained for wire-shape compatibility but no longer produced by any live
// code path.
type ChannelOverride struct {
	ChannelID      string   `json:"channelId"`
	RequireMention *bool    `json:"requireMention,omitempty"`
	InboundPrefix  *string  `json:"inboundPrefix,omitempty"`
	AllowedUsers   []string `json:"allowedUsers,omitempty"`
	AllowedRoles   []string `json:"allowedRoles,omitempty"`
}

// Envelope is the common wire shape for all shim↔router messages.
// Line-delimited JSON; one Envelope per line.
//
// Shim → router types: subscribe, unsubscribe, send,
// peers-list, peers-set-summary, task-get, task-update, task-handoff
// Router → shim types: subscribe-ack, inbound, error,
// peers-list-resp, peers-set-summary-resp, task-*-resp
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

	// Source identifies where an inbound envelope originated from. Set by
	// the daemon router when it fans out an event to the mesh-shim. Plan
	// 00-05 cut down the surviving sources to "peers" only; legacy
	// values ("discord", "whatsapp") may still appear in archived envelopes
	// but are no longer produced by any live code path.
	Source string `json:"source,omitempty"`

	// Target is the URL-shape destination the mesh-shim supplies on outbound
	// sends (e.g. "peers://agent/<name>"). The daemon router parses the
	// scheme and forwards to the matching backend. Plan 00-05 reduced the
	// supported scheme set to peers://; the field shape stays identical.
	Target string `json:"target,omitempty"`

	// inbound fields (router → shim)
	Sender    string `json:"sender,omitempty"`    // display name of the sender
	MessageID string `json:"messageId,omitempty"` // source-specific message id
	UserID    string `json:"userId,omitempty"`    // source-specific sender id
	Timestamp string `json:"ts,omitempty"`        // ISO-8601 message timestamp

	// peers inbound fields (broker → router → shim). PeerID is the sending
	// agent name; Team is set only for team-scoped sends.
	PeerID string `json:"peerId,omitempty"`
	Team   string `json:"team,omitempty"`

	// error fields (router → shim)
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
	MIMEType   string `json:"mimeType,omitempty"`   // MIME as reported by the source
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

// DialGateway opens a Unix-socket connection and returns a reader/writer
// pair. Predates Plan 00-05 — the function name is retained for callers
// (the mesh-shim dials the daemon's per-agent router socket); no separate
// gateway process exists since the channel-routing cut. The caller is
// responsible for closing the conn.
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
// populated. Allows producers to emit Claude Code's
// `notifications/claude/channel` with a proper meta block.
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

// InboundEnv builds the inbound envelope the router sends to a shim.
// channelID is the originating channel id; predates Plan 00-05 and remains
// for wire-shape compatibility.
func InboundEnv(routingKey, channelID, sender, content string) Envelope {
	return Envelope{V: Version, Type: "inbound", RoutingKey: routingKey, ChannelID: channelID, Sender: sender, Content: content}
}

// ErrorEnv builds an error envelope.
func ErrorEnv(reason, detail string) Envelope {
	return Envelope{V: Version, Type: "error", Reason: reason, Detail: detail}
}

// ListGroupsEnv builds the list-groups request. Predates Plan 00-05 and is
// retained for wire-shape compatibility; no live producer or consumer.
func ListGroupsEnv() Envelope {
	return Envelope{V: Version, Type: "list-groups"}
}

// ListGroupsRespEnv builds the list-groups-resp envelope. Predates Plan
// 00-05 and is retained for wire-shape compatibility; no live producer or
// consumer.
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

// TaskGetEnv builds the task-get request the shim sends to the router.
// RoutingKey carries the task UUID.
func TaskGetEnv(id string) Envelope {
	return Envelope{V: Version, Type: "task-get", RoutingKey: id}
}

// TaskGetRespEnv builds the task-get-resp the router sends back.
// On success Content holds the JSON-encoded task. On failure Reason is set.
func TaskGetRespEnv(content, errReason string) Envelope {
	return Envelope{V: Version, Type: "task-get-resp", Content: content, Reason: errReason}
}

// TaskUpdateEnv builds the task-update request. Content holds the
// JSON-encoded update payload (taskId + any mutable fields).
func TaskUpdateEnv(content string) Envelope {
	return Envelope{V: Version, Type: "task-update", Content: content}
}

// TaskUpdateRespEnv builds the task-update-resp. On success Reason is
// empty; on failure it carries the error string.
func TaskUpdateRespEnv(errReason string) Envelope {
	return Envelope{V: Version, Type: "task-update-resp", Reason: errReason}
}

// TaskHandoffEnv builds the task-handoff request. Content holds the
// JSON-encoded handoff payload (taskId, fromRole, toRole, ...).
func TaskHandoffEnv(content string) Envelope {
	return Envelope{V: Version, Type: "task-handoff", Content: content}
}

// TaskHandoffRespEnv builds the task-handoff-resp. On success Reason
// is empty; on failure it carries the error string.
func TaskHandoffRespEnv(errReason string) Envelope {
	return Envelope{V: Version, Type: "task-handoff-resp", Reason: errReason}
}
