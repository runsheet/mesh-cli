package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/spf13/cobra"
)

// discord channel-server flags. See spec 12 §1 / §5 for the schema; the flags
// mirror the template `config` keys so `mesh channel-server discord` can be
// spawned stand-alone for debugging.
var (
	discordAgentFlag          string
	discordIndexFlag          int
	discordChannelIDFlag      string
	discordTokenEnvFlag       string
	discordInboundPrefixFlag  string
	discordAllowedUsersFlag   string
	discordAllowedRolesFlag   string
	discordRequireMentionFlag bool
)

var channelServerDiscordCmd = &cobra.Command{
	Use:   "discord",
	Short: "Discord MCP channel server (spawned by claude via .claude/settings.json)",
	Long: "channel-server discord bridges a Discord text channel to an agent's " +
		"Claude Code session over MCP stdio. Inbound messages pass through the " +
		"allow-list filter (users, roles, require-mention) before being injected " +
		"as tool-poll results; outbound replies go through the discord_send_message " +
		"tool with 2000-char split handling.",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := discordServerConfig{
			Agent:          discordAgentFlag,
			Index:          discordIndexFlag,
			ChannelID:      discordChannelIDFlag,
			TokenEnv:       discordTokenEnvFlag,
			InboundPrefix:  discordInboundPrefixFlag,
			AllowedUsers:   splitCSV(discordAllowedUsersFlag),
			AllowedRoles:   splitCSV(discordAllowedRolesFlag),
			RequireMention: discordRequireMentionFlag,
		}
		return runDiscordChannelServer(cfg)
	},
}

func init() {
	f := channelServerDiscordCmd.Flags()
	f.StringVar(&discordAgentFlag, "agent", "", "Agent name this channel backs (required)")
	f.IntVar(&discordIndexFlag, "index", 0, "Channel index within the agent's channel list")
	f.StringVar(&discordChannelIDFlag, "channel-id", "", "Discord channel snowflake this instance owns (required)")
	f.StringVar(&discordTokenEnvFlag, "token-env", "", "Name of the env var holding the Discord bot token (required)")
	f.StringVar(&discordInboundPrefixFlag, "inbound-prefix", "user", `Source label on injected messages ("" to omit; default "user")`)
	f.StringVar(&discordAllowedUsersFlag, "allowed-users", "", "Comma-separated Discord user IDs to allow (empty = no user gating)")
	f.StringVar(&discordAllowedRolesFlag, "allowed-roles", "", "Comma-separated Discord role IDs to allow (empty = no role gating)")
	f.BoolVar(&discordRequireMentionFlag, "require-mention", false, "Only respond when the bot is @-mentioned")
	channelServerCmd.AddCommand(channelServerDiscordCmd)
}

// discordServerConfig is the fully-resolved runtime config for one discord
// channel-server instance. Values come from CLI flags (which in turn come
// from .claude/settings.json written by the daemon).
type discordServerConfig struct {
	Agent          string
	Index          int
	ChannelID      string
	TokenEnv       string
	InboundPrefix  string
	AllowedUsers   []string
	AllowedRoles   []string
	RequireMention bool
}

// discordMetrics is the set of live counters we publish to
// channels/discord-<index>.status.json so `mesh inspect` can surface them.
type discordMetrics struct {
	mu              sync.Mutex
	status          string // "connecting" | "connected" | "error: …"
	messagesIn      int
	messagesOut     int
	messagesDropped map[string]int
	botUserID       string
}

func newDiscordMetrics() *discordMetrics {
	return &discordMetrics{
		status:          "connecting",
		messagesDropped: map[string]int{},
	}
}

func (m *discordMetrics) setStatus(s string) {
	m.mu.Lock()
	m.status = s
	m.mu.Unlock()
}

func (m *discordMetrics) incIn() {
	m.mu.Lock()
	m.messagesIn++
	m.mu.Unlock()
}

func (m *discordMetrics) incOut() {
	m.mu.Lock()
	m.messagesOut++
	m.mu.Unlock()
}

func (m *discordMetrics) incDrop(reason string) {
	m.mu.Lock()
	m.messagesDropped[reason]++
	m.mu.Unlock()
}

func (m *discordMetrics) snapshot() ChannelStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	dropped := map[string]int{}
	for k, v := range m.messagesDropped {
		dropped[k] = v
	}
	return ChannelStatus{
		Type:            "discord",
		Status:          m.status,
		MessagesIn:      m.messagesIn,
		MessagesOut:     m.messagesOut,
		MessagesDropped: dropped,
	}
}

// pendingMessage is one inbound Discord message waiting to be delivered to
// claude via the discord_receive_next poll tool. FIFO; unbounded in-memory
// queue is fine for a single channel bridge (Discord's own rate limits cap
// throughput long before we'd exhaust RAM).
type pendingMessage struct {
	Content   string `json:"content"`
	Source    string `json:"source"`
	ChannelID string `json:"channel_id"`
	Username  string `json:"username"`
	Timestamp string `json:"timestamp"`
}

// discordInbox holds the FIFO queue of messages waiting for claude to poll.
// We use a channel + mutex-guarded slice so tools/call discord_receive_next
// can block up to a timeout without spinning.
type discordInbox struct {
	mu   sync.Mutex
	cond *sync.Cond
	q    []pendingMessage
}

func newDiscordInbox() *discordInbox {
	i := &discordInbox{}
	i.cond = sync.NewCond(&i.mu)
	return i
}

func (b *discordInbox) push(m pendingMessage) {
	b.mu.Lock()
	b.q = append(b.q, m)
	b.cond.Broadcast()
	b.mu.Unlock()
}

// pop returns the next message, blocking up to timeout. Returns ok=false on
// timeout or ctx cancel; the cancel path is signaled by the caller broadcasting
// on the cond before cancelling.
func (b *discordInbox) pop(ctx context.Context, timeout time.Duration) (pendingMessage, bool) {
	deadline := time.Now().Add(timeout)
	b.mu.Lock()
	defer b.mu.Unlock()

	// Spawn a watcher to wake us when ctx is cancelled.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			b.cond.Broadcast()
			b.mu.Unlock()
		case <-done:
		}
	}()
	defer close(done)

	for len(b.q) == 0 {
		if ctx.Err() != nil {
			return pendingMessage{}, false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return pendingMessage{}, false
		}
		// cond.Wait doesn't support deadlines natively; approximate with a
		// timed goroutine that broadcasts when the deadline passes.
		timer := time.AfterFunc(remaining, func() {
			b.mu.Lock()
			b.cond.Broadcast()
			b.mu.Unlock()
		})
		b.cond.Wait()
		timer.Stop()
	}
	msg := b.q[0]
	b.q = b.q[1:]
	return msg, true
}

func runDiscordChannelServer(cfg discordServerConfig) error {
	if cfg.Agent == "" {
		return fmt.Errorf("--agent is required")
	}
	if cfg.ChannelID == "" {
		return fmt.Errorf("--channel-id is required")
	}
	if cfg.TokenEnv == "" {
		return fmt.Errorf("--token-env is required")
	}

	token := os.Getenv(cfg.TokenEnv)
	if token == "" {
		fmt.Fprintf(os.Stderr, "mesh channel-server discord: env %s is empty; cannot connect\n", cfg.TokenEnv)
		os.Exit(2)
	}

	logFile, err := openChannelLogFile(cfg.Agent, "discord")
	if err != nil {
		return err
	}
	defer logFile.Close()
	logf := func(format string, args ...any) {
		fmt.Fprintf(logFile, "["+time.Now().UTC().Format(time.RFC3339Nano)+"] "+format+"\n", args...)
	}
	logf("discord channel start agent=%s index=%d channel=%s pid=%d",
		cfg.Agent, cfg.Index, cfg.ChannelID, os.Getpid())

	metrics := newDiscordMetrics()
	inbox := newDiscordInbox()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logf("received signal %s, shutting down", sig)
		cancel()
	}()

	// Status publisher: every 10 s (and once up-front) drop our metrics to the
	// side-file the daemon reads for `mesh inspect`.
	go runStatusPublisher(ctx, cfg, metrics, logf)

	// discordgo setup.
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		metrics.setStatus("error: " + err.Error())
		return fmt.Errorf("discordgo new: %w", err)
	}

	session.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsMessageContent |
		discordgo.IntentsDirectMessages
	if len(cfg.AllowedRoles) > 0 {
		// Role membership requires the privileged GuildMembers intent — warn
		// loudly so the operator knows to flip the toggle in the Discord
		// developer portal if they haven't.
		session.Identify.Intents |= discordgo.IntentsGuildMembers
		fmt.Fprintln(os.Stderr, "mesh channel-server discord: GuildMembers intent requested — "+
			"ensure it's enabled on the bot's developer-portal page")
	}

	// Capture the bot's own user id once the READY event lands so
	// shouldForward can drop self-authored messages.
	session.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		metrics.mu.Lock()
		metrics.botUserID = r.User.ID
		metrics.status = "connected"
		metrics.mu.Unlock()
		logf("discord ready bot=%s#%s id=%s", r.User.Username, r.User.Discriminator, r.User.ID)
		fmt.Fprintf(os.Stderr, "mesh channel-server discord: connected as %s (channel %s)\n",
			r.User.Username, cfg.ChannelID)
	})

	session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		handleDiscordMessage(s, m, cfg, metrics, inbox, logf)
	})

	if err := session.Open(); err != nil {
		metrics.setStatus("error: " + err.Error())
		return fmt.Errorf("discord open: %w", err)
	}
	defer session.Close()

	// MCP JSON-RPC loop on stdio. We serialize writes so the status publisher
	// (if it ever logs over MCP) and the handler loop don't interleave.
	var writeMu sync.Mutex
	write := func(msg jsonrpcMessage) error {
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

	reader := bufio.NewReader(os.Stdin)
	for {
		if ctx.Err() != nil {
			return nil
		}
		line, err := reader.ReadBytes('\n')
		trimmed := bytesTrimSpace(line)
		if len(trimmed) > 0 {
			logf("recv: %s", string(trimmed))
			if handleErr := handleDiscordMCP(ctx, trimmed, session, cfg, metrics, inbox, write, logf); handleErr != nil {
				logf("handle error: %v", handleErr)
			}
		}
		if err != nil {
			if err == io.EOF {
				logf("stdin EOF, exiting")
				return nil
			}
			return err
		}
	}
}

// runStatusPublisher drops the current metrics snapshot to the side-file every
// 10 s. First write happens immediately so `mesh inspect` has something to
// show during the few seconds before the first tick.
func runStatusPublisher(ctx context.Context, cfg discordServerConfig, metrics *discordMetrics, logf func(string, ...any)) {
	publish := func() {
		st := metrics.snapshot()
		st.ChannelID = cfg.ChannelID
		st.Index = cfg.Index
		if err := WriteChannelStatusFile(cfg.Agent, "discord", cfg.Index, st); err != nil {
			logf("status publish err: %v", err)
		}
	}
	publish()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Final publish so inspect sees an "disconnecting" / last state.
			publish()
			return
		case <-ticker.C:
			publish()
		}
	}
}

// handleDiscordMessage filters an inbound discordgo MessageCreate event and
// enqueues it for claude via the inbox if it survives the allow-list.
func handleDiscordMessage(
	s *discordgo.Session,
	m *discordgo.MessageCreate,
	cfg discordServerConfig,
	metrics *discordMetrics,
	inbox *discordInbox,
	logf func(string, ...any),
) {
	// Only listen to the configured channel (plus DMs when the channel arg
	// resolves to a DM channel id, which is unusual but valid).
	if m.ChannelID != cfg.ChannelID {
		return
	}

	ok, reason := shouldForward(s, m, cfg, metrics)
	if !ok {
		metrics.incDrop(reason)
		logf("drop author=%s reason=%s", m.Author.ID, reason)
		return
	}

	content := formatInboundContent(m, cfg.InboundPrefix)
	inbox.push(pendingMessage{
		Content:   content,
		Source:    "discord",
		ChannelID: m.ChannelID,
		Username:  m.Author.Username,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
	metrics.incIn()
	logf("forward author=%s content=%q", m.Author.Username, content)
}

// formatInboundContent applies the inbound-prefix rule:
//   - empty prefix → "<username>: <content>"
//   - non-empty    → "<prefix> @<username>: <content>"
func formatInboundContent(m *discordgo.MessageCreate, prefix string) string {
	username := m.Author.Username
	if prefix == "" {
		return fmt.Sprintf("%s: %s", username, m.Content)
	}
	return fmt.Sprintf("%s @%s: %s", prefix, username, m.Content)
}

// shouldForward implements spec 12 §5. Returns (true, "") on pass or (false,
// reason) on drop so callers can bump per-reason counters. Reasons are short
// identifiers used verbatim in the drop-counter map.
func shouldForward(
	s *discordgo.Session,
	m *discordgo.MessageCreate,
	cfg discordServerConfig,
	metrics *discordMetrics,
) (bool, string) {
	// Self-messages always drop — spec §5 bullet 5. We compare against the id
	// captured on READY; fall back to session.State.User.ID if available.
	metrics.mu.Lock()
	selfID := metrics.botUserID
	metrics.mu.Unlock()
	if selfID == "" && s != nil && s.State != nil && s.State.User != nil {
		selfID = s.State.User.ID
	}
	if selfID != "" && m.Author != nil && m.Author.ID == selfID {
		return false, "self"
	}

	// require-mention: explicit @-of-the-bot gate. Checked before the
	// user/role filters because mention-only implies "also needs to be
	// directed at me".
	if cfg.RequireMention {
		mentioned := false
		for _, u := range m.Mentions {
			if u.ID == selfID {
				mentioned = true
				break
			}
		}
		if !mentioned {
			return false, "no-mention"
		}
	}

	hasUserList := len(cfg.AllowedUsers) > 0
	hasRoleList := len(cfg.AllowedRoles) > 0

	switch {
	case !hasUserList && !hasRoleList:
		return true, ""

	case hasUserList && !hasRoleList:
		if contains(cfg.AllowedUsers, m.Author.ID) {
			return true, ""
		}
		return false, "user-not-allowed"

	case !hasUserList && hasRoleList:
		// DMs carry no Member; role membership is undefined. Drop with a
		// specific reason so `mesh inspect` shows the filter is doing its job.
		if m.Member == nil {
			return false, "role-not-available-in-dm"
		}
		for _, role := range m.Member.Roles {
			if contains(cfg.AllowedRoles, role) {
				return true, ""
			}
		}
		return false, "role-not-allowed"

	default:
		// OR semantics when both lists are present — either match passes.
		if contains(cfg.AllowedUsers, m.Author.ID) {
			return true, ""
		}
		if m.Member != nil {
			for _, role := range m.Member.Roles {
				if contains(cfg.AllowedRoles, role) {
					return true, ""
				}
			}
		}
		return false, "user-not-allowed"
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// handleDiscordMCP dispatches one JSON-RPC frame. The MCP surface exposes:
//
//   - initialize               (handshake)
//   - tools/list               (discord_send_message + discord_receive_next)
//   - tools/call               (dispatches to the two tools)
//
// discord_receive_next is a polling tool — claude calls it in a loop; we
// block up to 50 s per call waiting for a message, then return empty. The
// polling pattern sidesteps the risk that claude's MCP client rejects our
// chosen notification method shape (see commit body for the rationale).
func handleDiscordMCP(
	ctx context.Context,
	raw []byte,
	session *discordgo.Session,
	cfg discordServerConfig,
	metrics *discordMetrics,
	inbox *discordInbox,
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
				"name":    "mesh-channel-discord",
				"version": "0.1.0",
			},
		}})

	case "tools/list":
		tools := []map[string]any{
			{
				"name":        "discord_send_message",
				"description": "Post a message to the configured Discord channel. Content longer than 2000 chars is split on sentence/paragraph boundaries and sent as (i/n) parts.",
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
			{
				"name":        "discord_receive_next",
				"description": "Block up to ~50 s for the next Discord message, then return it. Returns an empty result when the wait times out; call again in a loop.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
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
		switch params.Name {
		case "discord_send_message":
			content, _ := params.Arguments["content"].(string)
			ids, err := sendDiscordMessage(session, cfg.ChannelID, content, metrics, logf)
			if err != nil {
				return write(jsonrpcMessage{
					ID:    msg.ID,
					Error: &jsonrpcError{Code: -32000, Message: err.Error()},
				})
			}
			result := map[string]any{
				"content": []map[string]any{{
					"type": "text",
					"text": fmt.Sprintf("sent %d message(s)", len(ids)),
				}},
				"messagesSent": len(ids),
				"messageIds":   ids,
			}
			return write(jsonrpcMessage{ID: msg.ID, Result: result})

		case "discord_receive_next":
			m, ok := inbox.pop(ctx, 50*time.Second)
			if !ok {
				return write(jsonrpcMessage{ID: msg.ID, Result: map[string]any{
					"content": []map[string]any{{"type": "text", "text": ""}},
					"empty":   true,
				}})
			}
			body, _ := json.Marshal(m)
			return write(jsonrpcMessage{ID: msg.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(body)}},
				"message": m,
			}})

		default:
			return write(jsonrpcMessage{
				ID:    msg.ID,
				Error: &jsonrpcError{Code: -32601, Message: "unknown tool"},
			})
		}

	default:
		return write(jsonrpcMessage{
			ID:    msg.ID,
			Error: &jsonrpcError{Code: -32601, Message: "method not found: " + msg.Method},
		})
	}
}

// sendDiscordMessage posts `content` to channelID, splitting into ≤2000-char
// parts on paragraph/sentence boundaries when necessary. Returns the ids of
// every message actually sent so the caller can surface them to the agent.
func sendDiscordMessage(
	session *discordgo.Session,
	channelID, content string,
	metrics *discordMetrics,
	logf func(string, ...any),
) ([]string, error) {
	if session == nil {
		return nil, fmt.Errorf("discord session not open")
	}
	parts := splitDiscordContent(content, 2000)
	ids := make([]string, 0, len(parts))
	for i, p := range parts {
		toSend := p
		if len(parts) > 1 {
			toSend = fmt.Sprintf("(%d/%d) %s", i+1, len(parts), p)
			// The (i/n) prefix plus a space adds up to at most 10 chars for
			// n<1000, so we slice again if the addition pushed us over.
			if len(toSend) > 2000 {
				toSend = toSend[:2000]
			}
		}
		msg, err := session.ChannelMessageSend(channelID, toSend)
		if err != nil {
			return ids, fmt.Errorf("send part %d/%d: %w", i+1, len(parts), err)
		}
		ids = append(ids, msg.ID)
		metrics.incOut()
		logf("sent part %d/%d id=%s len=%d", i+1, len(parts), msg.ID, len(toSend))
		// Small pause between parts — well under Discord's per-channel 5/sec
		// rate ceiling but enough to avoid burstiness if an agent spams.
		if i < len(parts)-1 {
			time.Sleep(250 * time.Millisecond)
		}
	}
	return ids, nil
}

// splitDiscordContent splits content into chunks ≤ limit bytes, preferring (in
// order) blank-line, single-newline, and sentence boundaries before falling
// back to a hard byte cut. We leave room for a `(i/n) ` prefix up to 10 chars;
// the actual prefix is added by the caller.
func splitDiscordContent(content string, limit int) []string {
	const prefixReserve = 10
	effective := limit - prefixReserve
	if len(content) <= limit {
		// Single-message path: return unchanged, no prefix needed.
		return []string{content}
	}

	var out []string
	remaining := content
	for len(remaining) > effective {
		cut := findBoundary(remaining, effective)
		if cut <= 0 {
			cut = effective
		}
		out = append(out, strings.TrimRight(remaining[:cut], " \t\n"))
		// Skip whitespace at the boundary so the next part doesn't start with
		// a stray separator.
		remaining = strings.TrimLeft(remaining[cut:], " \t\n")
	}
	if len(remaining) > 0 {
		out = append(out, remaining)
	}
	return out
}

// findBoundary locates the best break point in s[:max]: a blank line, then
// a single newline, then a sentence terminator followed by a space, then
// the last space. Returns -1 if no good boundary exists (caller hard-cuts).
func findBoundary(s string, max int) int {
	if len(s) <= max {
		return len(s)
	}
	window := s[:max]
	if idx := strings.LastIndex(window, "\n\n"); idx > 0 {
		return idx + 2
	}
	if idx := strings.LastIndex(window, "\n"); idx > 0 {
		return idx + 1
	}
	for _, sep := range []string{". ", "! ", "? "} {
		if idx := strings.LastIndex(window, sep); idx > 0 {
			return idx + len(sep)
		}
	}
	if idx := strings.LastIndex(window, " "); idx > 0 {
		return idx + 1
	}
	return -1
}

// splitCSV splits a CSV flag value into trimmed, non-empty tokens.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

