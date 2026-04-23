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

var (
	discordShimAgentFlag   string
	discordShimIndexFlag   int
	discordShimAccountFlag string
)

var channelServerDiscordShimCmd = &cobra.Command{
	Use:   "discord-shim",
	Short: "Discord MCP shim (spawned by claude via .claude/settings.json)",
	Long: "discord-shim speaks MCP JSON-RPC on stdio and bridges to the " +
		"daemon-supervised discord-gateway over gateway.sock. One shim per " +
		"(agent, channel) pair; the gateway holds the upstream Discord connection. " +
		"Configuration is read from ~/.mesh/agents/<agent>/channels/discord-<index>.config.json.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if discordShimAgentFlag == "" {
			return fmt.Errorf("--agent is required")
		}
		if discordShimAccountFlag == "" {
			return fmt.Errorf("--account is required")
		}
		return runDiscordShim(discordShimAgentFlag, discordShimIndexFlag, discordShimAccountFlag)
	},
}

func init() {
	f := channelServerDiscordShimCmd.Flags()
	f.StringVar(&discordShimAgentFlag, "agent", "", "Agent name (required)")
	f.IntVar(&discordShimIndexFlag, "index", 0, "Channel index within the agent's channel list")
	f.StringVar(&discordShimAccountFlag, "account", "", "Discord account name (required)")
	channelServerCmd.AddCommand(channelServerDiscordShimCmd)
}

// discordShimConfig is the full runtime config for a shim, loaded from the
// per-shim config file written by WriteClaudeSettings.
type discordShimConfig struct {
	SubscriptionMode string                  `json:"subscriptionMode"`
	Account          string                  `json:"account"`
	ChannelID        string                  `json:"channelId,omitempty"`
	GuildID          string                  `json:"guildId,omitempty"`
	RequireMention   bool                    `json:"requireMention"`
	AllowedUsers     []string                `json:"allowedUsers,omitempty"`
	AllowedRoles     []string                `json:"allowedRoles,omitempty"`
	InboundPrefix    string                  `json:"inboundPrefix,omitempty"`
	MultiTool        bool                    `json:"multiTool"`
	ChannelOverrides []discordChannelOverride `json:"channelOverrides,omitempty"`
}

func loadDiscordShimConfig(agentName string, index int) (discordShimConfig, error) {
	path, err := discordShimConfigFilePath(agentName, index)
	if err != nil {
		return discordShimConfig{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return discordShimConfig{}, fmt.Errorf("read shim config %s: %w", path, err)
	}
	var cfg discordShimConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return discordShimConfig{}, fmt.Errorf("parse shim config %s: %w", path, err)
	}
	if cfg.SubscriptionMode == "" {
		cfg.SubscriptionMode = "channel"
	}
	return cfg, nil
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
		Type:        "discord",
		Index:       index,
		Status:      m.status,
		ChannelID:   channelID,
		MessagesIn:  m.messagesIn,
		MessagesOut: m.messagesOut,
	}
}

// lastInbound tracks the most recent inbound channelId for guild-mode shims.
// The shim uses it as the default reply destination when discord_send_message
// is called without an explicit channelId argument.
type lastInbound struct {
	mu        sync.Mutex
	channelID string
}

func (l *lastInbound) set(id string) {
	l.mu.Lock()
	l.channelID = id
	l.mu.Unlock()
}

func (l *lastInbound) get() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.channelID
}

func runDiscordShim(agentName string, index int, account string) error {
	cfg, err := loadDiscordShimConfig(agentName, index)
	if err != nil {
		return fmt.Errorf("load shim config: %w", err)
	}
	// account on CLI takes precedence over config file for forward-compat,
	// but use config file account if CLI didn't supply one.
	if account == "" {
		account = cfg.Account
	}

	logFile, err := openChannelLogFile(agentName, "discord-shim")
	if err != nil {
		return err
	}
	defer logFile.Close()
	logf := func(format string, args ...any) {
		fmt.Fprintf(logFile, "["+time.Now().UTC().Format(time.RFC3339Nano)+"] "+format+"\n", args...)
	}

	routingID := cfg.ChannelID
	if cfg.SubscriptionMode == "guild" {
		routingID = cfg.GuildID
	}
	logf("discord shim start agent=%s index=%d account=%s mode=%s routingKey=%s pid=%d",
		agentName, index, account, cfg.SubscriptionMode, routingID, os.Getpid())

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

	// Connect to gateway. The daemon starts the gateway subprocess before
	// autostarting agents, but discordgo needs ~1s to connect to Discord and
	// bind the gateway socket — so the shim may race ahead of socket bind.
	// Retry for up to 30s with 200ms intervals before giving up.
	sockPath, err := DiscordGatewaySockPath(account)
	if err != nil {
		return fmt.Errorf("gateway sock path: %w", err)
	}
	var (
		gwConn net.Conn
		gwRd   *ipcReader
		gwWr   *ipcWriter
	)
	deadline := time.Now().Add(30 * time.Second)
	for {
		gwConn, gwRd, gwWr, err = dialGateway(sockPath)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			metrics.setStatus("error: " + err.Error())
			publishShimStatus(agentName, index, cfg, metrics)
			logf("gateway dial deadline exceeded: %v", err)
			return fmt.Errorf("connect to gateway (30s deadline): %w", err)
		}
		logf("gateway dial retry: %v", err)
		time.Sleep(200 * time.Millisecond)
	}
	defer gwConn.Close()
	logf("connected to gateway sock=%s", sockPath)

	// Build and send subscribe envelope.
	var sub ipcEnvelope
	switch cfg.SubscriptionMode {
	case "guild":
		overrides := make([]ipcChannelOverride, 0, len(cfg.ChannelOverrides))
		for _, ov := range cfg.ChannelOverrides {
			iov := ipcChannelOverride{
				ChannelID:      ov.ChannelID,
				RequireMention: ov.RequireMention,
				InboundPrefix:  ov.InboundPrefix,
				AllowedUsers:   ov.AllowedUsers,
				AllowedRoles:   ov.AllowedRoles,
			}
			overrides = append(overrides, iov)
		}
		sub = ipcSubscribeGuildEnv(agentName, index, cfg.GuildID, cfg.InboundPrefix,
			cfg.AllowedUsers, cfg.AllowedRoles, cfg.RequireMention, overrides)
	default:
		sub = ipcSubscribeChannelEnv(agentName, index, cfg.ChannelID, cfg.InboundPrefix,
			cfg.AllowedUsers, cfg.AllowedRoles, cfg.RequireMention)
	}

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
	logf("subscribed routingKey=%s", routingID)
	metrics.setStatus("connected")

	// Status publisher.
	go func() {
		publishShimStatus(agentName, index, cfg, metrics)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				publishShimStatus(agentName, index, cfg, metrics)
				return
			case <-ticker.C:
				publishShimStatus(agentName, index, cfg, metrics)
			}
		}
	}()

	// lastInbound tracks the originating channelId of the most recent inbound
	// message. Used by guild-mode shims to pick the default reply channel.
	last := &lastInbound{}

	// Inbound messages from gateway.
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
		toolName = fmt.Sprintf("discord_send_message_%d", index)
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
				// Track the originating channel for guild-mode reply routing.
				if env.ChannelID != "" {
					last.set(env.ChannelID)
				}
				logf("inbound routingKey=%s channelId=%s sender=%s content=%q",
					env.RoutingKey, env.ChannelID, env.Sender, env.Content)
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
			if herr := handleDiscordShimMCP(ctx, trimmed, agentName, index, cfg, toolName, gwWr, metrics, last, writeMCP, logf); herr != nil {
				logf("handle err: %v", herr)
			}
		}
		if err != nil {
			if err == io.EOF {
				logf("stdin EOF, exiting")
				_ = gwWr.Write(ipcUnsubscribeEnv(routingID))
				return nil
			}
			return err
		}
	}
}

func handleDiscordShimMCP(
	ctx context.Context,
	raw []byte,
	agentName string,
	index int,
	cfg discordShimConfig,
	toolName string,
	gwWr *ipcWriter,
	metrics *shimMetrics,
	last *lastInbound,
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
		// Echo whatever protocolVersion the client requested; falls back to a
		// known-good string if the client omits it. MCP spec allows negotiating
		// a lower version but Claude Code 2.x refuses to use servers that
		// respond with an older protocol than it asked for.
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
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    "mesh-channel-discord-shim",
				"version": "0.2.0",
			},
		}})

	case "tools/list":
		tools := buildDiscordShimTools(toolName, cfg)
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

		// Resolve the target channelId.
		targetChannelID, mcpErr := resolveTargetChannelID(cfg, params.Arguments, last)
		if mcpErr != "" {
			return write(jsonrpcMessage{
				ID:    msg.ID,
				Error: &jsonrpcError{Code: -32000, Message: mcpErr},
			})
		}

		if err := gwWr.Write(ipcSendEnv(targetChannelID, content)); err != nil {
			return write(jsonrpcMessage{
				ID:    msg.ID,
				Error: &jsonrpcError{Code: -32000, Message: "gateway send: " + err.Error()},
			})
		}
		metrics.incOut()
		logf("sent via gateway channelId=%s len=%d", targetChannelID, len(content))
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

// resolveTargetChannelID determines the destination channel for an outbound
// message.
//
// Channel mode: always the configured channelId (implicit, no arg needed).
// Guild mode: prefer explicit channelId arg → fall back to last-inbound →
// error if neither is available.
func resolveTargetChannelID(cfg discordShimConfig, args map[string]any, last *lastInbound) (string, string) {
	if cfg.SubscriptionMode != "guild" {
		return cfg.ChannelID, ""
	}
	// Explicit arg takes precedence.
	if explicit, ok := args["channelId"].(string); ok && explicit != "" {
		return explicit, ""
	}
	// Fall back to last inbound.
	if id := last.get(); id != "" {
		return id, ""
	}
	return "", "guild mode: no channelId arg and no prior inbound message to reply to"
}

// buildDiscordShimTools returns the tools/list payload for this shim.
func buildDiscordShimTools(toolName string, cfg discordShimConfig) []map[string]any {
	var description string
	if cfg.SubscriptionMode == "guild" {
		description = fmt.Sprintf(
			"Post a message to a Discord channel in guild %s. "+
				"content is required. "+
				"channelId is optional: if omitted, the reply goes to the channel that sent the most recent inbound message. "+
				"Supply channelId explicitly for proactive (unsolicited) sends. "+
				"Returns an error if channelId is omitted and no inbound has been received yet.",
			cfg.GuildID,
		)
	} else {
		description = fmt.Sprintf(
			"Post a message to Discord channel %s. "+
				"Content longer than 2000 chars is split on sentence/paragraph boundaries and sent as (i/n) parts.",
			cfg.ChannelID,
		)
	}

	props := map[string]any{
		"content": map[string]any{
			"type":        "string",
			"description": "Text to post.",
		},
	}
	required := []string{"content"}

	if cfg.SubscriptionMode == "guild" {
		props["channelId"] = map[string]any{
			"type":        "string",
			"description": "Discord channel snowflake to post to. Optional: defaults to the channel from the most recent inbound message.",
		}
	}

	return []map[string]any{
		{
			"name":        toolName,
			"description": description,
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		},
	}
}

func publishShimStatus(agentName string, index int, cfg discordShimConfig, metrics *shimMetrics) {
	displayID := cfg.ChannelID
	if cfg.SubscriptionMode == "guild" {
		displayID = cfg.GuildID
	}
	st := metrics.snapshot(displayID, index)
	_ = WriteChannelStatusFile(agentName, "discord", index, st)
}
