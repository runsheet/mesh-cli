package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"projectmesh.io/mesh-cli/internal/ipc"
	"projectmesh.io/mesh-cli/internal/paths"
	"projectmesh.io/mesh-cli/internal/version"
)

// shimMetrics tracks per-shim counters held in-process. Plan 00-05 removed
// the per-agent channel-status side-file (and its sole reader,
// channels.ChannelStatusesForAgent), so these counters are no longer
// published anywhere — they remain useful for the in-process incOut hook
// passed to the mesh tool handlers and as future-proofing for the Phase 2
// dispatcher rebuild.
type shimMetrics struct {
	mu          sync.Mutex
	status      string
	messagesIn  int
	messagesOut int
}

func (m *shimMetrics) setStatus(s string) {
	m.mu.Lock()
	m.status = s
	m.mu.Unlock()
}

func (m *shimMetrics) incIn()  { m.mu.Lock(); m.messagesIn++; m.mu.Unlock() }
func (m *shimMetrics) incOut() { m.mu.Lock(); m.messagesOut++; m.mu.Unlock() }

// checkParentPlugin reads /proc/<ppid>/cmdline and verifies the parent claude
// process was launched for agentName.
func checkParentPlugin(agentName string, logf func(string, ...any)) bool {
	ppid := os.Getppid()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", ppid))
	if err != nil {
		return true
	}
	parentAgent := ParentPluginAgent(string(data))
	if parentAgent == "" {
		return true
	}
	if parentAgent != agentName {
		logf("agent mismatch: shim --agent=%s but parent claude (pid %d) loaded plugin:%s@mesh — exiting to avoid misrouted inbound",
			agentName, ppid, parentAgent)
		return false
	}
	return true
}

// Run is the mesh-shim entry point. It speaks MCP JSON-RPC on stdio and
// bridges to the daemon's per-agent router socket.
func Run(agentName string) error {
	logFile, err := OpenChannelLogFile(agentName, "mesh-shim")
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

	metrics := &shimMetrics{status: "connecting"}
	last := &MeshLastInbound{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logf("signal %s, shutting down", sig)
		cancel()
	}()

	sockPath, err := paths.RouterSockPath(agentName)
	if err != nil {
		return fmt.Errorf("router sock path: %w", err)
	}
	routerConn, routerRd, routerWr, err := ipc.DialGateway(sockPath)
	for err != nil {
		deadline := time.Now().Add(30 * time.Second)
		if time.Now().After(deadline) {
			metrics.setStatus("error: " + err.Error())
			logf("router dial deadline exceeded: %v", err)
			return fmt.Errorf("connect to router (30s deadline): %w", err)
		}
		logf("router dial retry: %v", err)
		time.Sleep(200 * time.Millisecond)
		routerConn, routerRd, routerWr, err = ipc.DialGateway(sockPath)
	}
	defer routerConn.Close()
	logf("connected to router sock=%s", sockPath)

	subEnv := ipc.Envelope{
		V:          ipc.Version,
		Type:       "subscribe",
		Agent:      agentName,
		RoutingKey: "agent:" + agentName,
	}
	if err := routerWr.Write(subEnv); err != nil {
		return fmt.Errorf("router subscribe: %w", err)
	}

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

	pendingResp := make(chan ipc.Envelope, 1)

	inbound := make(chan ipc.Envelope, 64)
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
					logf("resp dropped (no waiter): type=%s", env.Type)
				}
			}
		}
	}()

	var (
		lastRecvMu sync.Mutex
		lastRecv   time.Time
	)
	setLastRecv := func(t time.Time) {
		lastRecvMu.Lock()
		lastRecv = t
		lastRecvMu.Unlock()
	}
	getLastRecv := func() time.Time {
		lastRecvMu.Lock()
		defer lastRecvMu.Unlock()
		return lastRecv
	}

	var writeMu sync.Mutex
	writeMCP := func(msg JSONRPCMessage) error {
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
				if source == "peers" {
					if env.PeerID != "" {
						last.Set(source, env.PeerID)
					}
				} else if env.ChannelID != "" {
					last.Set(source, env.ChannelID)
				} else if env.RoutingKey != "" {
					last.Set(source, env.RoutingKey)
				}

				logf("INBOUND source=%s channelId=%s sender=%q content=%q",
					source, env.ChannelID, env.Sender, env.Content)

				inboundAt := time.Now()
				lastRecvSnapshot := getLastRecv()
				go func() {
					time.Sleep(5 * time.Second)
					if getLastRecv().Equal(lastRecvSnapshot) || getLastRecv().Before(inboundAt) {
						logf("WATCHDOG: no claude activity 5s after inbound — claude may be ignoring notifications/claude/channel")
					}
				}()

				meta := map[string]any{
					"source": source,
				}
				if source == "peers" {
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
				content := FormatInboundContent(env.Content, env.Attachments)
				if content == "" {
					continue
				}
				params, _ := json.Marshal(map[string]any{
					"content": content,
					"meta":    meta,
				})
				_ = writeMCP(JSONRPCMessage{
					Method: "notifications/claude/channel",
					Params: params,
				})
			}
		}
	}()

	reader := bufio.NewReader(os.Stdin)
	for {
		if ctx.Err() != nil {
			return nil
		}
		line, err := reader.ReadBytes('\n')
		trimmed := BytesTrimSpace(line)
		if len(trimmed) > 0 {
			setLastRecv(time.Now())
			logf("recv: %s", string(trimmed))
			if herr := handleMeshShimMCP(ctx, trimmed, agentName, routerWr, pendingResp, metrics, last, writeMCP, logf); herr != nil {
				logf("handle err: %v", herr)
			}
		}
		if err != nil {
			if err == io.EOF {
				logf("stdin EOF, exiting")
				_ = routerWr.Write(ipc.UnsubscribeEnv("agent:" + agentName))
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
	routerWr *ipc.Writer,
	pendingResp chan ipc.Envelope,
	metrics *shimMetrics,
	last *MeshLastInbound,
	write func(JSONRPCMessage) error,
	logf func(string, ...any),
) error {
	var msg JSONRPCMessage
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
		return write(JSONRPCMessage{ID: msg.ID, Result: map[string]any{
			"protocolVersion": proto,
			"capabilities": map[string]any{
				"tools": map[string]any{},
				"experimental": map[string]any{
					"claude/channel": map[string]any{},
				},
			},
			"serverInfo": map[string]any{
				"name":    "mesh",
				"version": version.Version,
			},
			"instructions": "Messages from peers arrive as notifications/claude/channel with meta.source identifying the source (e.g. \"peers\"). " +
				"Use mesh_send(target, content) to send a message — target is a URL-shape string like \"peers://agent/<name>\" or \"peers://team/<name>\". " +
				"Use mesh_reply(content) to reply to the most recent inbound message. " +
				"If a message doesn't need a reply (e.g. a bot post you see as a loop guard), stay silent.",
		}})

	case "tools/list":
		tools := BuildMeshShimToolsForAgent(agentName)
		return write(JSONRPCMessage{ID: msg.ID, Result: map[string]any{"tools": tools}})

	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return write(JSONRPCMessage{
				ID:    msg.ID,
				Error: &JSONRPCError{Code: -32602, Message: "invalid params"},
			})
		}

		switch params.Name {
		case "mesh_send":
			return HandleMeshSend(ctx, msg.ID, params.Arguments, routerWr, metrics.incOut, logf, write)
		case "mesh_reply":
			return HandleMeshReply(ctx, msg.ID, params.Arguments, routerWr, metrics.incOut, last, logf, write)
		case "mesh_list_peers":
			return HandleMeshListPeers(ctx, msg.ID, params.Arguments, routerWr, pendingResp, write, logf)
		case "mesh_set_summary":
			return HandleMeshSetSummary(ctx, msg.ID, params.Arguments, routerWr, pendingResp, write, logf)
		case "mesh_recall":
			return HandleMeshRecall(ctx, msg.ID, params.Arguments, agentName, write, logf)
		case "mesh_remember":
			return HandleMeshRemember(ctx, msg.ID, params.Arguments, agentName, write, logf)
		default:
			return write(JSONRPCMessage{
				ID:    msg.ID,
				Error: &JSONRPCError{Code: -32601, Message: "unknown tool: " + params.Name},
			})
		}

	default:
		return write(JSONRPCMessage{
			ID:    msg.ID,
			Error: &JSONRPCError{Code: -32601, Message: "method not found: " + msg.Method},
		})
	}
}
