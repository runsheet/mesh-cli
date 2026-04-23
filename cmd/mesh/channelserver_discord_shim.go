package main

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

	"github.com/spf13/cobra"
)

var (
	discordShimAgentFlag      string
	discordShimIndexFlag      int
	discordShimAccountFlag    string
	discordShimChannelIDFlag  string
	discordShimPrefixFlag     string
	discordShimAllowedUsers   string
	discordShimAllowedRoles   string
	discordShimRequireMention bool
	// multiTool: when true the tool is named discord_send_message_<index>
	// (agent has multiple discord channels). Set by mcpsettings based on count.
	discordShimMultiToolFlag bool
)

var channelServerDiscordShimCmd = &cobra.Command{
	Use:   "discord-shim",
	Short: "Discord MCP shim (spawned by claude via .claude/settings.json)",
	Long: "discord-shim speaks MCP JSON-RPC on stdio and bridges to the " +
		"daemon-supervised discord-gateway over gateway.sock. One shim per " +
		"(agent, channel) pair; the gateway holds the upstream Discord connection.",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := discordShimConfig{
			Agent:          discordShimAgentFlag,
			Index:          discordShimIndexFlag,
			Account:        discordShimAccountFlag,
			ChannelID:      discordShimChannelIDFlag,
			InboundPrefix:  discordShimPrefixFlag,
			AllowedUsers:   splitCSV(discordShimAllowedUsers),
			AllowedRoles:   splitCSV(discordShimAllowedRoles),
			RequireMention: discordShimRequireMention,
			MultiTool:      discordShimMultiToolFlag,
		}
		return runDiscordShim(cfg)
	},
}

func init() {
	f := channelServerDiscordShimCmd.Flags()
	f.StringVar(&discordShimAgentFlag, "agent", "", "Agent name (required)")
	f.IntVar(&discordShimIndexFlag, "index", 0, "Channel index within the agent's channel list")
	f.StringVar(&discordShimAccountFlag, "account", "", "Discord account name (required)")
	f.StringVar(&discordShimChannelIDFlag, "channel-id", "", "Discord channel snowflake this shim owns (required)")
	f.StringVar(&discordShimPrefixFlag, "inbound-prefix", "user", `Source label prepended to inbound messages`)
	f.StringVar(&discordShimAllowedUsers, "allowed-users", "", "Comma-separated Discord user IDs to allow")
	f.StringVar(&discordShimAllowedRoles, "allowed-roles", "", "Comma-separated Discord role IDs to allow")
	f.BoolVar(&discordShimRequireMention, "require-mention", false, "Only forward messages that @-mention the bot")
	f.BoolVar(&discordShimMultiToolFlag, "multi-tool", false, "Suffix tool name with index (agent has multiple discord channels)")
	channelServerCmd.AddCommand(channelServerDiscordShimCmd)
}

type discordShimConfig struct {
	Agent          string
	Index          int
	Account        string
	ChannelID      string
	InboundPrefix  string
	AllowedUsers   []string
	AllowedRoles   []string
	RequireMention bool
	MultiTool      bool
}

// shimMetrics tracks per-shim counters for the status.json side-file.
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
func (m *shimMetrics) incIn() { m.mu.Lock(); m.messagesIn++; m.mu.Unlock() }
func (m *shimMetrics) incOut() { m.mu.Lock(); m.messagesOut++; m.mu.Unlock() }
func (m *shimMetrics) snapshot(channelID string, index int) ChannelStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ChannelStatus{
		Type:      "discord",
		Index:     index,
		Status:    m.status,
		ChannelID: channelID,
		MessagesIn:  m.messagesIn,
		MessagesOut: m.messagesOut,
	}
}

func runDiscordShim(cfg discordShimConfig) error {
	if cfg.Agent == "" {
		return fmt.Errorf("--agent is required")
	}
	if cfg.Account == "" {
		return fmt.Errorf("--account is required")
	}
	if cfg.ChannelID == "" {
		return fmt.Errorf("--channel-id is required")
	}

	logFile, err := openChannelLogFile(cfg.Agent, "discord-shim")
	if err != nil {
		return err
	}
	defer logFile.Close()
	logf := func(format string, args ...any) {
		fmt.Fprintf(logFile, "["+time.Now().UTC().Format(time.RFC3339Nano)+"] "+format+"\n", args...)
	}
	logf("discord shim start agent=%s index=%d account=%s channelId=%s pid=%d",
		cfg.Agent, cfg.Index, cfg.Account, cfg.ChannelID, os.Getpid())

	metrics := &shimMetrics{status: "connecting"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logf("signal %s, shutting down", sig)
		cancel()
	}()

	// Connect to gateway.
	sockPath, err := DiscordGatewaySockPath(cfg.Account)
	if err != nil {
		return fmt.Errorf("gateway sock path: %w", err)
	}
	gwConn, gwRd, gwWr, err := dialGateway(sockPath)
	if err != nil {
		metrics.setStatus("error: " + err.Error())
		publishShimStatus(cfg, metrics)
		return fmt.Errorf("connect to gateway: %w", err)
	}
	defer gwConn.Close()

	// Send subscribe.
	sub := ipcSubscribeEnv(cfg.Agent, cfg.Index, cfg.ChannelID, cfg.InboundPrefix,
		cfg.AllowedUsers, cfg.AllowedRoles, cfg.RequireMention)
	if err := gwWr.Write(sub); err != nil {
		return fmt.Errorf("gateway subscribe: %w", err)
	}

	// Wait for ack.
	ack, err := gwRd.Read()
	if err != nil {
		return fmt.Errorf("gateway ack read: %w", err)
	}
	if ack.Type == "error" {
		return fmt.Errorf("gateway subscribe error: %s: %s", ack.Reason, ack.Detail)
	}
	if ack.Type != "subscribe-ack" {
		return fmt.Errorf("unexpected gateway response: %s", ack.Type)
	}
	logf("subscribed routingKey=%s", cfg.ChannelID)
	metrics.setStatus("connected")

	// Status publisher.
	go func() {
		publishShimStatus(cfg, metrics)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				publishShimStatus(cfg, metrics)
				return
			case <-ticker.C:
				publishShimStatus(cfg, metrics)
			}
		}
	}()

	// Inbound messages from gateway land here and get forwarded to claude as
	// MCP notifications.
	inbound := make(chan ipcEnvelope, 64)
	go func() {
		for {
			env, err := gwRd.Read()
			if err != nil {
				logf("gateway read err: %v", err)
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

	// MCP tool name: unsuffixed for single-channel agents, _<index> for multi.
	toolName := "discord_send_message"
	if cfg.MultiTool {
		toolName = fmt.Sprintf("discord_send_message_%d", cfg.Index)
	}

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

	// Forward inbound messages from gateway to claude as MCP notifications.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-inbound:
				metrics.incIn()
				logf("inbound sender=%s content=%q", env.Sender, env.Content)
				params, _ := json.Marshal(map[string]any{
					"level": "info",
					"data":  env.Content,
				})
				_ = writeMCP(jsonrpcMessage{
					Method: "notifications/message",
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
			logf("recv: %s", string(trimmed))
			if herr := handleDiscordShimMCP(ctx, trimmed, cfg, toolName, gwWr, metrics, writeMCP, logf); herr != nil {
				logf("handle err: %v", herr)
			}
		}
		if err != nil {
			if err == io.EOF {
				logf("stdin EOF, exiting")
				// Unsubscribe cleanly so the gateway removes the routing entry.
				_ = gwWr.Write(ipcUnsubscribeEnv(cfg.ChannelID))
				return nil
			}
			return err
		}
	}
}

func handleDiscordShimMCP(
	ctx context.Context,
	raw []byte,
	cfg discordShimConfig,
	toolName string,
	gwWr *ipcWriter,
	metrics *shimMetrics,
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
		return write(jsonrpcMessage{ID: msg.ID, Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    "mesh-channel-discord-shim",
				"version": "0.2.0",
			},
		}})

	case "tools/list":
		tools := []map[string]any{
			{
				"name":        toolName,
				"description": fmt.Sprintf("Post a message to the configured Discord channel (channelId=%s). Content longer than 2000 chars is split on sentence/paragraph boundaries and sent as (i/n) parts.", cfg.ChannelID),
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{
							"type":        "string",
							"description": "Text to post.",
						},
					},
					"required": []string{"content"},
				},
			},
		}
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
		if params.Name != toolName {
			return write(jsonrpcMessage{
				ID:    msg.ID,
				Error: &jsonrpcError{Code: -32601, Message: "unknown tool: " + params.Name},
			})
		}
		content, _ := params.Arguments["content"].(string)
		// Forward to gateway.
		if err := gwWr.Write(ipcSendEnv(cfg.ChannelID, content)); err != nil {
			return write(jsonrpcMessage{
				ID:    msg.ID,
				Error: &jsonrpcError{Code: -32000, Message: "gateway send: " + err.Error()},
			})
		}
		metrics.incOut()
		logf("sent via gateway channelId=%s len=%d", cfg.ChannelID, len(content))
		return write(jsonrpcMessage{ID: msg.ID, Result: map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": "sent",
			}},
		}})

	default:
		return write(jsonrpcMessage{
			ID:    msg.ID,
			Error: &jsonrpcError{Code: -32601, Message: "method not found: " + msg.Method},
		})
	}
}

func publishShimStatus(cfg discordShimConfig, metrics *shimMetrics) {
	st := metrics.snapshot(cfg.ChannelID, cfg.Index)
	_ = WriteChannelStatusFile(cfg.Agent, "discord", cfg.Index, st)
}
