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
	"path/filepath"
	"strings"
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

// parentPluginAgent parses a null-byte-delimited /proc/<pid>/cmdline string and
// returns the agent name embedded in a --dangerously-load-development-channels
// flag of the form plugin:<agent>@mesh. Returns "" if the flag is absent or the
// cmdline is malformed.
func parentPluginAgent(cmdline string) string {
	const needle = "plugin:"
	const suffix = "@mesh"
	for _, arg := range strings.Split(cmdline, "\x00") {
		// The flag may appear as a single token
		// "--dangerously-load-development-channels plugin:<agent>@mesh"
		// or split across two tokens; handle both.
		val := arg
		if strings.HasPrefix(arg, "--dangerously-load-development-channels=") {
			val = strings.TrimPrefix(arg, "--dangerously-load-development-channels=")
		}
		if strings.HasPrefix(val, needle) && strings.HasSuffix(val, suffix) {
			return val[len(needle) : len(val)-len(suffix)]
		}
	}
	return ""
}

// checkParentPlugin reads /proc/<ppid>/cmdline and verifies the parent claude
// process was launched for agentName. If the cmdline cannot be read (macOS,
// sandbox, permission denied) the check is skipped and true is returned.
// Returns false only when the cmdline is readable and names a different agent.
func checkParentPlugin(agentName string, logf func(string, ...any)) bool {
	ppid := os.Getppid()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", ppid))
	if err != nil {
		// Not Linux, or process already gone — skip check.
		return true
	}
	parentAgent := parentPluginAgent(string(data))
	if parentAgent == "" {
		// Flag not present (e.g. parent is a shell or test runner) — skip.
		return true
	}
	if parentAgent != agentName {
		logf("agent mismatch: shim --agent=%s but parent claude (pid %d) loaded plugin:%s@mesh — exiting to avoid misrouted inbound",
			agentName, ppid, parentAgent)
		return false
	}
	return true
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

	logf("mesh shim start agent=%s pid=%d ppid=%d", agentName, os.Getpid(), os.Getppid())

	if !checkParentPlugin(agentName, logf) {
		return fmt.Errorf("agent mismatch: shim --agent=%s spawned under wrong parent claude process; see log for details", agentName)
	}

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

	// pendingResp carries synchronous router responses to in-flight tool calls
	// (peers-list-resp, peers-set-summary-resp). MCP tool calls are sequential,
	// so a single unbuffered channel is sufficient — no correlation ID needed.
	pendingResp := make(chan ipcEnvelope, 1)

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
			switch env.Type {
			case "inbound":
				select {
				case inbound <- env:
				case <-ctx.Done():
					return
				}
			case "peers-list-resp", "peers-set-summary-resp":
				select {
				case pendingResp <- env:
				default:
					logf("peers resp dropped (no waiter): type=%s", env.Type)
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
				content := formatInboundContent(env.Content, env.Attachments)
				if content == "" {
					continue
				}
				params, _ := json.Marshal(map[string]any{
					"content": content,
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
			if herr := handleMeshShimMCP(ctx, trimmed, agentName, routerWr, pendingResp, metrics, last, writeMCP, logf); herr != nil {
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
	pendingResp chan ipcEnvelope,
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
			return handleMeshListPeers(ctx, msg.ID, params.Arguments, routerWr, pendingResp, write, logf)
		case "mesh_set_summary":
			return handleMeshSetSummary(ctx, msg.ID, params.Arguments, routerWr, pendingResp, write, logf)
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

// shimAttachment is the parsed form of one entry in the MCP attachments array.
type shimAttachment struct {
	Path     string `json:"path"`
	Type     string `json:"type"`
	MIMEType string // derived from extension during validation
}

// parseMeshSendAttachments parses and validates the attachments field from MCP
// args. agentName is used by validateAttachmentPath. Returns the parsed list
// (nil if none) or a non-empty error string for the tool response.
func parseMeshSendAttachments(args map[string]any, agentName string) ([]shimAttachment, string) {
	raw, ok := args["attachments"]
	if !ok || raw == nil {
		return nil, ""
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, "attachments must be an array"
	}
	if len(list) == 0 {
		return nil, ""
	}
	if len(list) > 1 {
		return nil, "only one attachment supported in v1"
	}

	item, ok := list[0].(map[string]any)
	if !ok {
		return nil, "attachment item must be an object"
	}
	attPath, _ := item["path"].(string)
	attType, _ := item["type"].(string)

	if attType != "image" {
		return nil, "unsupported attachment type in v1: " + attType
	}

	// Extension → MIME allowlist.
	ext := strings.ToLower(filepath.Ext(attPath))
	mime, ok := outboundImageExts[ext]
	if !ok {
		return nil, "unsupported image type: " + ext
	}

	// Path validation: existence, size, allowed roots.
	info, err := os.Stat(attPath)
	if err != nil {
		return nil, "attachment file not found: " + attPath
	}
	if info.Size() > outboundMaxBytes {
		return nil, fmt.Sprintf("attachment too large: %d bytes (max 8 MB)", info.Size())
	}
	if err := validateAttachmentPath(attPath, agentName); err != nil {
		return nil, err.Error()
	}

	return []shimAttachment{{Path: attPath, Type: attType, MIMEType: mime}}, ""
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

	// Parse and validate attachments (agentName not available in this scope;
	// peers sends cannot carry attachments — attachment support is discord/whatsapp only).
	// agentName is not threaded through to here; use empty string so the agent
	// workdir root is not checked (peers:// sends cannot have attachments per spec).
	shimAtts, attErr := parseMeshSendAttachments(args, "")
	if attErr != "" {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32602, Message: "mesh_send: " + attErr},
		})
	}

	if content == "" && len(shimAtts) == 0 {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32602, Message: "mesh_send: content or attachment required"},
		})
	}

	var ipcAtts []ipcAttachment
	for _, a := range shimAtts {
		ipcAtts = append(ipcAtts, ipcAttachment{Type: a.Type, Path: a.Path, MIMEType: a.MIMEType})
	}

	env := ipcSendEnvWithAttachments(target, "", content, ipcAtts)
	if err := routerWr.Write(env); err != nil {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32000, Message: "router send: " + err.Error()},
		})
	}
	metrics.incOut()
	logf("mesh_send target=%s len=%d attachments=%d", target, len(content), len(ipcAtts))
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

	// Validate attachments early so we can give a clear error before checking reply target.
	shimAtts, attErr := parseMeshSendAttachments(args, "")
	if attErr != "" {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32602, Message: "mesh_reply: " + attErr},
		})
	}

	if content == "" && len(shimAtts) == 0 {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32602, Message: "mesh_reply: content or attachment required"},
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
	// Forward with attachments preserved.
	fwdArgs := map[string]any{"target": target, "content": content}
	if atts, ok := args["attachments"]; ok {
		fwdArgs["attachments"] = atts
	}
	return handleMeshSend(ctx, id, fwdArgs, routerWr, metrics, logf, write)
}

// handleMeshListPeers handles the mesh_list_peers(team?) tool call.
// Sends a peers-list request to the router (daemon process) and waits for
// the peers-list-resp on pendingResp. Block-write-block-read is safe here
// because MCP tool calls are sequential — only one is in flight at a time.
func handleMeshListPeers(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	routerWr *ipcWriter,
	pendingResp chan ipcEnvelope,
	write func(jsonrpcMessage) error,
	logf func(string, ...any),
) error {
	team, _ := args["team"].(string)
	if err := routerWr.Write(ipcPeersListEnv(team)); err != nil {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32000, Message: "router peers-list: " + err.Error()},
		})
	}
	select {
	case resp := <-pendingResp:
		logf("mesh_list_peers team=%q resp=%s", team, resp.Content)
		return write(jsonrpcMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": resp.Content,
			}},
		}})
	case <-ctx.Done():
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32000, Message: "mesh_list_peers: context cancelled"},
		})
	}
}

// handleMeshSetSummary handles the mesh_set_summary(summary) tool call.
// Sends a peers-set-summary request to the router (daemon process) and waits
// for the peers-set-summary-resp on pendingResp. Same sequential-MCP guarantee
// applies — one request in flight at a time.
func handleMeshSetSummary(
	ctx context.Context,
	id json.RawMessage,
	args map[string]any,
	routerWr *ipcWriter,
	pendingResp chan ipcEnvelope,
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
	if err := routerWr.Write(ipcPeersSetSummaryEnv(summary)); err != nil {
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32000, Message: "router peers-set-summary: " + err.Error()},
		})
	}
	select {
	case resp := <-pendingResp:
		if resp.Reason != "" {
			logf("mesh_set_summary err: %s", resp.Reason)
			return write(jsonrpcMessage{
				ID:    id,
				Error: &jsonrpcError{Code: -32000, Message: "mesh_set_summary: " + resp.Reason},
			})
		}
		logf("mesh_set_summary ok len=%d", len(summary))
		return write(jsonrpcMessage{ID: id, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": "summary updated",
			}},
		}})
	case <-ctx.Done():
		return write(jsonrpcMessage{
			ID:    id,
			Error: &jsonrpcError{Code: -32000, Message: "mesh_set_summary: context cancelled"},
		})
	}
}

// buildMeshShimTools returns the tools/list payload for the mesh shim.
func buildMeshShimTools() []map[string]any {
	return []map[string]any{
		{
			"name": "mesh_send",
			"description": "Send a message (optionally with an image attachment) to a channel. target is a URL-shape string: " +
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
						"description": "Message text. Becomes the caption when an attachment is present. May be empty if an attachment is supplied.",
					},
					"attachments": map[string]any{
						"type":        "array",
						"description": "Optional. At most one item in v1.",
						"maxItems":    1,
						"items": map[string]any{
							"type":     "object",
							"required": []string{"path", "type"},
							"properties": map[string]any{
								"path": map[string]any{
									"type":        "string",
									"description": "Absolute path to a local image file.",
								},
								"type": map[string]any{
									"type":        "string",
									"enum":        []string{"image"},
									"description": "Media kind. Only 'image' is supported in v1.",
								},
							},
						},
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
						"description": "Reply text. May be empty if an attachment is supplied.",
					},
					"attachments": map[string]any{
						"type":        "array",
						"description": "Optional. At most one item in v1.",
						"maxItems":    1,
						"items": map[string]any{
							"type":     "object",
							"required": []string{"path", "type"},
							"properties": map[string]any{
								"path": map[string]any{
									"type":        "string",
									"description": "Absolute path to a local image file.",
								},
								"type": map[string]any{
									"type":        "string",
									"enum":        []string{"image"},
									"description": "Media kind. Only 'image' is supported in v1.",
								},
							},
						},
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

// formatInboundContent builds the final content string sent to Claude.
// Caption is the raw message text; attachments carry downloaded media.
//
// Happy path:  "[<type>: /abs/path]"          e.g. "[image: …]", "[audio: …]"
// Failure:     "[<type>: download_failed reason=…]"
// Unknown type uses "image" as the label for back-compat with pre-audio clients.
// Non-allowlisted MIMEs arrive with FailReason set and Type=="image"; they
// continue to emit "[image: download_failed …]" unchanged (OQ-3 from spec 16).
func formatInboundContent(caption string, attachments []ipcAttachment) string {
	content := caption
	for _, att := range attachments {
		label := att.Type
		if label == "" {
			label = "image" // back-compat: pre-audio attachments have no Type
		}
		var marker string
		if att.FailReason != "" {
			marker = "[" + label + ": download_failed reason=" + att.FailReason + "]"
		} else if att.Path != "" {
			marker = "[" + label + ": " + att.Path + "]"
		}
		if marker == "" {
			continue
		}
		if content != "" {
			content += "\n"
		}
		content += marker
	}
	return content
}

// publishMeshShimStatus writes the shim's current metrics to the status file.
func publishMeshShimStatus(agentName string, metrics *meshShimMetrics) {
	st := metrics.snapshot()
	_ = WriteChannelStatusFile(agentName, "mesh", 0, st)
}
