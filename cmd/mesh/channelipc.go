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

// ipcEnvelope is the common wire shape for all shim↔gateway messages over the
// gateway Unix socket. Line-delimited JSON; one envelope per line.
//
// Shim → gateway types: subscribe, unsubscribe, send
// Gateway → shim types: subscribe-ack, inbound, error
type ipcEnvelope struct {
	V    int    `json:"v"`
	Type string `json:"type"`

	// subscribe fields (shim → gateway)
	Agent          string   `json:"agent,omitempty"`
	Index          int      `json:"index,omitempty"`
	RoutingKey     string   `json:"routingKey,omitempty"`
	AllowedUsers   []string `json:"allowedUsers,omitempty"`
	AllowedRoles   []string `json:"allowedRoles,omitempty"`
	RequireMention bool     `json:"requireMention,omitempty"`
	InboundPrefix  string   `json:"inboundPrefix,omitempty"`

	// send fields (shim → gateway)
	Content string `json:"content,omitempty"`

	// inbound fields (gateway → shim)
	Sender string `json:"sender,omitempty"`

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

// ipcSubscribeEnv builds the subscribe envelope a shim sends on connect.
func ipcSubscribeEnv(agent string, index int, routingKey, inboundPrefix string, allowedUsers, allowedRoles []string, requireMention bool) ipcEnvelope {
	return ipcEnvelope{
		V:              ipcVersion,
		Type:           "subscribe",
		Agent:          agent,
		Index:          index,
		RoutingKey:     routingKey,
		InboundPrefix:  inboundPrefix,
		AllowedUsers:   allowedUsers,
		AllowedRoles:   allowedRoles,
		RequireMention: requireMention,
	}
}

// ipcSendEnv builds the send envelope a shim forwards to the gateway.
func ipcSendEnv(routingKey, content string) ipcEnvelope {
	return ipcEnvelope{V: ipcVersion, Type: "send", RoutingKey: routingKey, Content: content}
}

// ipcUnsubscribeEnv builds the unsubscribe envelope.
func ipcUnsubscribeEnv(routingKey string) ipcEnvelope {
	return ipcEnvelope{V: ipcVersion, Type: "unsubscribe", RoutingKey: routingKey}
}

// ipcAckEnv builds the subscribe-ack the gateway sends back.
func ipcAckEnv(routingKey string) ipcEnvelope {
	return ipcEnvelope{V: ipcVersion, Type: "subscribe-ack", RoutingKey: routingKey}
}

// ipcInboundEnv builds the inbound envelope the gateway sends to a shim.
func ipcInboundEnv(routingKey, sender, content string) ipcEnvelope {
	return ipcEnvelope{V: ipcVersion, Type: "inbound", RoutingKey: routingKey, Sender: sender, Content: content}
}

// ipcErrorEnv builds an error envelope.
func ipcErrorEnv(reason, detail string) ipcEnvelope {
	return ipcEnvelope{V: ipcVersion, Type: "error", Reason: reason, Detail: detail}
}
