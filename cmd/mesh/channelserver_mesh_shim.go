package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// lastRecv tracks the timestamp of the most recent frame received from claude
// on stdin. Used by the watchdog goroutine to detect claude ignoring
// notifications/claude/channel rather than processing it.
var (
	lastRecvMu sync.Mutex
	lastRecv   time.Time
)

func setLastRecvAt(t time.Time) {
	lastRecvMu.Lock()
	lastRecv = t
	lastRecvMu.Unlock()
}

func lastRecvAt() time.Time {
	lastRecvMu.Lock()
	defer lastRecvMu.Unlock()
	return lastRecv
}

var meshShimAgentFlag string

var channelServerMeshShimCmd = &cobra.Command{
	Use:   "mesh-shim",
	Short: "Unified mesh MCP shim (spawned by claude via plugin .mcp.json)",
	Long: "mesh-shim speaks MCP JSON-RPC on stdio and bridges to the daemon's " +
		"per-agent router socket (~/.mesh/agents/<name>/mesh.sock). One shim per " +
		"agent; the router multiplexes across all platform gateways.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if meshShimAgentFlag == "" {
			return fmt.Errorf("--agent is required")
		}
		return runMeshShim(meshShimAgentFlag)
	},
}

func init() {
	channelServerMeshShimCmd.Flags().StringVar(&meshShimAgentFlag, "agent", "", "Agent name (required)")
	channelServerCmd.AddCommand(channelServerMeshShimCmd)
}

// meshLastInbound records the platform and routing info needed to reply to the
// most recent inbound message across all sources.
type meshLastInbound struct {
	mu     sync.Mutex
	source string // e.g. "discord"
	// For discord: the originating channelId (used to construct the reply target).
	channelID string
}

func (l *meshLastInbound) set(source, channelID string) {
	l.mu.Lock()
	l.source = source
	l.channelID = channelID
	l.mu.Unlock()
}

func (l *meshLastInbound) replyTarget() (string, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.source == "" {
		return "", ""
	}
	switch l.source {
	case "discord":
		if l.channelID != "" {
			return l.source, "discord://channel/" + l.channelID
		}
	case "whatsapp":
		if l.channelID != "" {
			return l.source, "whatsapp://chat/" + l.channelID
		}
	case "peers":
		if l.channelID != "" {
			return l.source, "peers://agent/" + l.channelID
		}
	}
	return l.source, ""
}

// meshShimMetrics tracks per-shim counters for the status.json side-file.
type meshShimMetrics struct {
	mu          sync.Mutex
	status      string
	messagesIn  int
	messagesOut int
}

func (m *meshShimMetrics) setStatus(s string) {
	m.mu.Lock()
	m.status = s
	m.mu.Unlock()
}

func (m *meshShimMetrics) incIn() { m.mu.Lock(); m.messagesIn++; m.mu.Unlock() }
func (m *meshShimMetrics) incOut() { m.mu.Lock(); m.messagesOut++; m.mu.Unlock() }

func (m *meshShimMetrics) snapshot() ChannelStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ChannelStatus{
		Type:        "mesh",
		Index:       0,
		Status:      m.status,
		MessagesIn:  m.messagesIn,
		MessagesOut: m.messagesOut,
	}
}

func runMeshShim(agentName string) error {
	logFile, err := openChannelLogFile(agentName, "mesh-shim")
	if err != nil {
		return err
	}
	defer logFile.Close()
	logf := func(format string, args ...any) {
		fmt.Fprintf(logFile, "["+time.Now().UTC().Format(time.RFC3339Nano)+"] "+format+"\n", args...)
	}

	logf("mesh shim start agent=%s pid=%d", agentName, os.Getpid())

	metrics := &meshShimMetrics{status: "connecting"}
	last := &meshLastInbound{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logf("signal %s, shutting down", sig)
		cancel()
	}()

	// Connect to the per-agent router socket. The daemon starts the router
	// before auto-starting agents, but there may be a brief race. Retry for
	// up to 30s.
	sockPath, err := MeshRouterSockPath(agentName)
	if err != nil {
		return fmt.Errorf("router sock path: %w", err)
	}
	var (
		routerConn net.Conn
		routerRd   *ipcReader
		routerWr   *ipcWriter
	)
	deadline := time.Now().Add(30 * time.Second)
	for {
		routerConn, routerRd, routerWr, err = dialGateway(sockPath)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			metrics.setStatus("error: " + err.Error())
			logf("router dial deadline exceeded: %v", err)
			return fmt.Errorf("connect to router (30s deadline): %w", err)
		}
		logf("router dial retry: %v", err)
		time.Sleep(200 * time.Millisecond)
	}
	defer routerConn.Close()
	logf("connected to router sock=%s", sockPath)

	// Send subscribe envelope. The router uses agent name to look up subscriptions.
	subEnv := ipcEnvelope{
		V:          ipcVersion,
		Type:       "subscribe",
		Agent:      agentName,
		RoutingKey: "agent:" + agentName,
	}
	if err := routerWr.Write(subEnv); err != nil {
		return fmt.Errorf("router subscribe: %w", err)
	}

	// Wait for ack.
	ack, err := routerRd.Read()
	if err != nil {
		return fmt.Errorf("router ack read: %w", err)
	}
	if ack.Type == "error" {
		return fmt.Errorf("router subscribe error: %s: %s", ack.Reason, ack.Detail)
	}
	if ack.Type != "subscribe-ack" {
		return fmt.Errorf("unexpected router response: %s", ack.Type)
	}
	logf("subscribed to router agent=%s", agentName)
	metrics.setStatus("connected")

	// Publish status.
	go func() {
		publishMeshShimStatus(agentName, metrics)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				publishMeshShimStatus(agentName, metrics)
				return
			case <-ticker.C:
				publishMeshShimStatus(agentName, metrics)
			}
		}
	}()

	// Inbound messages from router.
	inbound := make(chan ipcEnvelope, 64)
	go func() {
		for {
			env, err := routerRd.Read()
			if err != nil {
				logf("router read err: %v", err)
				cancel()
				return
			}
			if env.Type == "inbound" {
				select {
				case inbound <- env:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// Serialize MCP writes.
	var writeMu sync.Mutex
	writeMCP := func(msg jsonrpcMessage) error {
		msg.JSONRPC = "2.0"
		writeMu.Lock()
		defer writeMu.Unlock()
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		logf("send: %s", string(data))
		_, err = os.Stdout.Write(append(data, '\n'))
		return err
	}

	// Forward inbound messages from router to claude as MCP notifications.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-inbound:
				metrics.incIn()
				source := env.Source
				if source == "" {
					source = "unknown"
				}
				// Track last inbound for mesh_reply.
				if source == "peers" {
					if env.PeerID != "" {
						last.set(source, env.PeerID)
					}
				} else if env.ChannelID != "" {
					last.set(source, env.ChannelID)
				} else if env.RoutingKey != "" {
					last.set(source, env.RoutingKey)
				}

				logf("INBOUND source=%s channelId=%s sender=%q content=%q",
					source, env.ChannelID, env.Sender, env.Content)

				// Watchdog: if claude doesn't respond within 5s, log a warning.
				inboundAt := time.Now()
				lastRecvSnapshot := lastRecvAt()
				go func() {
					time.Sleep(5 * time.Second)
					if lastRecvAt().Equal(lastRecvSnapshot) || lastRecvAt().Before(inboundAt) {
						logf("WATCHDOG: no claude activity 5s after inbound — claude may be ignoring notifications/claude/channel")
					}
				}()

				meta := map[string]any{
					"source": source,
				}
				if source == "peers" {
					// Peers inbound: omit chat_id/message_id; carry peer_id and
					// optionally team.
					if env.PeerID != "" {
						meta["peer_id"] = env.PeerID
					}
					if env.Team != "" {
						meta["team"] = env.Team
					}
					if env.Timestamp != "" {
						meta["ts"] = env.Timestamp
					}
				} else {
					if env.ChannelID != "" {
						meta["chat_id"] = env.ChannelID
					}
					if env.MessageID != "" {
						meta["message_id"] = env.MessageID
					}
					if env.Sender != "" {
						meta["user"] = env.Sender
					}
					if env.UserID != "" {
						meta["user_id"] = env.UserID
					}
					if env.Timestamp != "" {
						meta["ts"] = env.Timestamp
					}
				}
				params, _ := json.Marshal(map[string]any{
					"content": env.Content,
					"meta":    meta,
				})
				_ = writeMCP(jsonrpcMessage{
					Method: "notifications/claude/channel",
					Params: params,
				})
			}
		}
	}()

	// MCP JSON-RPC loop on stdin.
	reader := bufio.NewReader(os.Stdin)
	for {
		if ctx.Err() != nil {
			return nil
		}
		line, err := reader.ReadBytes('\n')
		trimmed := bytesTrimSpace(line)
		if len(trimmed) > 0 {
			setLastRecvAt(time.Now())
			logf("recv: %s", string(trimmed))
			if herr := handleMeshShimMCP(ctx, trimmed, agentName, routerWr, metrics, last, writeMCP, logf); herr != nil {
				logf("handle err: %v", herr)
			}
		}
		if err != nil {
			if err == io.EOF {
				logf("stdin EOF, exiting")
				_ = routerWr.Write(ipcUnsubscribeEnv("agent:" + agentName))
				return nil
			}
			return err
		}
	}
}

func handleMeshShimMCP(
	ctx context.Context,
	raw []byte,
	agentName string,
	routerWr *ipcWriter,
	metrics *meshShimMetrics,
	last *meshLastInbound,
	write func(jsonrpcMessage) error,
	logf func(string, ...any),
) error {
	var msg jsonrpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return fmt.Errorf("parse frame: %w", err)
	}
	if len(msg.ID) == 0 {
		return nil
	}

	switch msg.Method {
	case "initialize":
		var initParams struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &initParams)
		proto := initParams.ProtocolVersion
		if proto == "" {
			proto = "2025-06-18"
		}
		return write(jsonrpcMessage{ID: msg.ID, Result: map[string]any{
			"protocolVersion": proto,
			"capabilities": map[string]any{
				"tools": map[string]any{},
				"experimental": map[string]any{
					"claude/channel": map[string]any{},
				},
			},
			"serverInfo": map[string]any{
				"name":    "mesh",
				"version": "0.2.0",
			},
			"instructions": "Messages from channels arrive as notifications/claude/channel with meta.source identifying the platform (e.g. \"discord\"). " +
				"Use mesh_send(target, content) to send a message — target is a URL-shape string like \"discord://channel/<channelId>\" or \"discord://dm/<userId>\". " +
				"Use mesh_reply(content) to reply to the most recent inbound message. " +
				"If a message doesn't need a reply (e.g. a bot post you see as a loop guard), stay silent.",
		}})

	case "tools/list":
		tools := buildMeshShimTools()
		return write(jsonrpcMessage{ID: msg.ID, Result: map[string]any{"tools": tools}})

	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return write(jsonrpcMessage{
				ID:    msg.ID,
				Error: &jsonrpcError{Code: -32602, Message: "invalid params"},
			})
		}

		switch params.Name {
		case "mesh_send":
			return handleMeshSend(ctx, msg.ID, params.Arguments, routerWr, metrics, logf, write)
		case "mesh_reply":
			return handleMeshReply(ctx, msg.ID, params.Arguments, routerWr, metrics, last, logf, write)
		case "mesh_list_peers":
			return handleMeshListPeers(msg.ID, params.Arguments, write)
		case "mesh_set_summary":
			return handleMeshSetSummary(msg.ID, params.Arguments, agentName, write, logf)
		default:
			return write(jsonrpcMessage{
				ID:    msg.ID,
				Error: &jsonrpcError{Code: -32601, Message: "unknown tool: " + params.Name},
			})
		}

	default:
		return write(jsonrpcMessage{
			ID:    msg.ID,
			Error: &jsonrpcError{Code: -32601, Message: "method not found: " + msg.Method},
		})
	}
}

// handleMeshSend handles the mesh_send(target, content) tool call.
func handleMeshSend(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	routerWr *ipcWriter,
	metrics *meshShimMetrics,
	logf func(string, ...any),
	write func(jsonrpcMessage) error,
) error {
	target, _ := args["target"].(string)
	content, _ := args["content"].(string)

	if target == "" {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32602, Message: "mesh_send: target is required"},
		})
	}
	if content == "" {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32602, Message: "mesh_send: content is required"},
		})
	}

	// Validate the scheme before forwarding.
	platform, _, _, parseErr := parseTargetURL(target)
	if parseErr != "" {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32602, Message: "mesh_send: " + parseErr},
		})
	}

	switch platform {
	case "discord", "whatsapp", "peers":
		// valid
	default:
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32602, Message: "mesh_send: unknown scheme " + platform + " (supported: discord://, whatsapp://, peers://)"},
		})
	}

	env := ipcEnvelope{
		V:       ipcVersion,
		Type:    "send",
		Target:  target,
		Content: content,
	}
	if err := routerWr.Write(env); err != nil {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32000, Message: "router send: " + err.Error()},
		})
	}
	metrics.incOut()
	logf("mesh_send target=%s len=%d", target, len(content))
	return write(jsonrpcMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": "sent",
		}},
	}})
}

// handleMeshReply handles the mesh_reply(content) tool call. It uses the
// most recent inbound to construct the reply target and delegates to
// handleMeshSend.
func handleMeshReply(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	routerWr *ipcWriter,
	metrics *meshShimMetrics,
	last *meshLastInbound,
	logf func(string, ...any),
	write func(jsonrpcMessage) error,
) error {
	content, _ := args["content"].(string)
	if content == "" {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32602, Message: "mesh_reply: content is required"},
		})
	}

	source, target := last.replyTarget()
	if target == "" {
		msg := "mesh_reply: no prior inbound to reply to"
		if source != "" {
			msg = "mesh_reply: cannot construct reply target for source " + source
		}
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32000, Message: msg},
		})
	}

	logf("mesh_reply source=%s target=%s", source, target)
	return handleMeshSend(ctx, id, map[string]any{"target": target, "content": content}, routerWr, metrics, logf, write)
}

// handleMeshListPeers handles the mesh_list_peers(team?) tool call. Queries
// the in-daemon peers broker directly (in-process).
func handleMeshListPeers(
	id json.RawMessage,
	args map[string]any,
	write func(jsonrpcMessage) error,
) error {
	team, _ := args["team"].(string)
	peers := globalPeersBroker.ListPeers(team)

	items := make([]map[string]any, 0, len(peers))
	for _, p := range peers {
		item := map[string]any{
			"peer_id":      p.PeerID,
			"node_id":      p.NodeID,
			"teams":        p.Teams,
			"last_seen_at": p.LastSeenAt.UTC().Format(time.RFC3339),
		}
		if p.Summary != "" {
			item["summary"] = p.Summary
		}
		items = append(items, item)
	}

	data, _ := json.Marshal(items)
	return write(jsonrpcMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": string(data),
		}},
	}})
}

// handleMeshSetSummary handles the mesh_set_summary(summary) tool call. Updates
// the calling agent's summary in the peers broker.
func handleMeshSetSummary(
	id json.RawMessage,
	args map[string]any,
	agentName string,
	write func(jsonrpcMessage) error,
	logf func(string, ...any),
) error {
	summary, _ := args["summary"].(string)
	if summary == "" {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32602, Message: "mesh_set_summary: summary is required"},
		})
	}
	ok := globalPeersBroker.SetSummary(agentName, summary)
	if !ok {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32000, Message: "mesh_set_summary: agent not registered with peers broker (add a peers channel to your template)"},
		})
	}
	logf("mesh_set_summary agent=%s len=%d", agentName, len(summary))
	return write(jsonrpcMessage{ID: id, Result: map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": "summary updated",
		}},
	}})
}

// buildMeshShimTools returns the tools/list payload for the mesh shim.
func buildMeshShimTools() []map[string]any {
	return []map[string]any{
		{
			"name": "mesh_send",
			"description": "Send a message to a channel. target is a URL-shape string: " +
				"discord://channel/<channelId> to send to a Discord channel, " +
				"discord://dm/<userId> to open or reuse a DM with a user, " +
				"peers://agent/<name> to send to a specific peer agent, " +
				"peers://team/<name> to broadcast to all members of a team (excluding self), " +
				"peers://broadcast to reach all reachable peers (excluding self).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{
						"type":        "string",
						"description": "URL-shape destination, e.g. discord://channel/1234567890 or peers://agent/nova",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Text content to send.",
					},
				},
				"required": []string{"target", "content"},
			},
		},
		{
			"name":        "mesh_reply",
			"description": "Reply to the most recent inbound message. Equivalent to calling mesh_send with the source channel as target. Fails if no inbound has been received yet.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "Reply text.",
					},
				},
				"required": []string{"content"},
			},
		},
		{
			"name":        "mesh_list_peers",
			"description": "List reachable peers registered with this daemon. Optional filter by team.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"team": map[string]any{
						"type":        "string",
						"description": "Filter by team name. Omit to list all peers.",
					},
				},
			},
		},
		{
			"name":        "mesh_set_summary",
			"description": "Publish a short self-description visible to other peers via mesh_list_peers.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{
						"type":        "string",
						"description": "Short description of this agent's current focus or capabilities.",
					},
				},
				"required": []string{"summary"},
			},
		},
	}
}

// publishMeshShimStatus writes the shim's current metrics to the status file.
func publishMeshShimStatus(agentName string, metrics *meshShimMetrics) {
	st := metrics.snapshot()
	_ = WriteChannelStatusFile(agentName, "mesh", 0, st)
}
