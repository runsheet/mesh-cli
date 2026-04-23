package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/spf13/cobra"
)

var discordGatewayAccountFlag string

var channelServerDiscordGatewayCmd = &cobra.Command{
	Use:   "discord-gateway",
	Short: "Discord gateway (daemon-supervised; one per account)",
	Long: "discord-gateway holds the upstream discordgo connection for a paired " +
		"Discord account and serves shim connections over gateway.sock. " +
		"Normally spawned by the mesh daemon's ChannelManager.",
	RunE: func(cmd *cobra.Command, args []string) error {
		account := strings.TrimSpace(discordGatewayAccountFlag)
		if account == "" {
			return fmt.Errorf("--account is required")
		}
		return runDiscordGateway(account)
	},
}

func init() {
	channelServerDiscordGatewayCmd.Flags().StringVar(&discordGatewayAccountFlag, "account", "", "Discord account name (required)")
	channelServerCmd.AddCommand(channelServerDiscordGatewayCmd)
}

// shimConn represents one connected shim. RoutingKey is the channelId the
// shim owns; filter config was sent in the subscribe envelope.
type shimConn struct {
	conn           net.Conn
	wr             *ipcWriter
	agent          string
	index          int
	routingKey     string
	allowedUsers   []string
	allowedRoles   []string
	requireMention bool
	inboundPrefix  string
}

// discordGateway holds runtime state for one gateway process.
type discordGateway struct {
	account string

	mu       sync.Mutex
	shims    map[string]*shimConn // keyed by routingKey (channelId)
	botID    string
	status   string
	msgIn    int
	msgOut   int
	dropped  map[string]int // keyed by reason
	session  *discordgo.Session
}

func newDiscordGateway(account string) *discordGateway {
	return &discordGateway{
		account: account,
		shims:   map[string]*shimConn{},
		status:  "connecting",
		dropped: map[string]int{},
	}
}

func runDiscordGateway(account string) error {
	token, err := LoadDiscordToken(account)
	if err != nil {
		return fmt.Errorf("load token: %w", err)
	}
	token = strings.TrimSpace(token)

	logf := gatewayLogf(account)
	logf("discord gateway start account=%s pid=%d", account, os.Getpid())

	gw := newDiscordGateway(account)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logf("signal %s received, shutting down", sig)
		cancel()
	}()

	// discordgo session
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return fmt.Errorf("discordgo new: %w", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsMessageContent |
		discordgo.IntentsDirectMessages

	gw.session = dg

	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		gw.mu.Lock()
		gw.botID = r.User.ID
		gw.status = "connected"
		meta, _ := LoadDiscordAccountMeta(account)
		meta.BotUserID = r.User.ID
		_ = UpdateDiscordAccountMeta(account, meta)
		gw.mu.Unlock()
		logf("discord ready bot=%s#%s id=%s", r.User.Username, r.User.Discriminator, r.User.ID)
		fmt.Fprintf(os.Stderr, "mesh discord-gateway: connected as %s (account=%s)\n", r.User.Username, account)
		gw.publishStatus(account, r.User.Username)
	})

	dg.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		gw.handleInbound(s, m, logf)
	})

	if err := dg.Open(); err != nil {
		gw.mu.Lock()
		gw.status = "error: " + err.Error()
		gw.mu.Unlock()
		gw.publishStatus(account, "")
		return fmt.Errorf("discord open: %w", err)
	}
	defer dg.Close()

	// Listen on gateway.sock for shim connections.
	sockPath, err := DiscordGatewaySockPath(account)
	if err != nil {
		return fmt.Errorf("gateway sock path: %w", err)
	}
	_ = os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	defer func() {
		listener.Close()
		_ = os.Remove(sockPath)
	}()
	if err := os.Chmod(sockPath, 0o600); err != nil {
		return fmt.Errorf("chmod gateway.sock: %w", err)
	}
	logf("gateway.sock listening at %s", sockPath)

	// Status publisher.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		gw.publishStatus(account, "")
		for {
			select {
			case <-ctx.Done():
				gw.publishStatus(account, "")
				return
			case <-ticker.C:
				gw.publishStatus(account, "")
			}
		}
	}()

	// Accept shim connections.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				logf("accept error: %v", err)
				continue
			}
			go gw.handleShim(ctx, conn, account, logf)
		}
	}()

	<-ctx.Done()
	logf("discord gateway shutting down")
	return nil
}

// handleShim serves one shim connection until the connection closes or ctx
// is cancelled.
func (gw *discordGateway) handleShim(ctx context.Context, conn net.Conn, account string, logf func(string, ...any)) {
	defer conn.Close()
	rd := newIPCReader(conn)
	wr := newIPCWriter(conn)
	var shim *shimConn

	defer func() {
		if shim != nil {
			gw.mu.Lock()
			if gw.shims[shim.routingKey] == shim {
				delete(gw.shims, shim.routingKey)
				logf("shim disconnected agent=%s index=%d routingKey=%s", shim.agent, shim.index, shim.routingKey)
			}
			gw.mu.Unlock()
			gw.publishStatus(account, "")
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		env, err := rd.Read()
		if err != nil {
			return
		}

		switch env.Type {
		case "subscribe":
			if env.RoutingKey == "" {
				_ = wr.Write(ipcErrorEnv("bad-subscribe", "routingKey is required"))
				return
			}
			gw.mu.Lock()
			if existing, taken := gw.shims[env.RoutingKey]; taken && existing != shim {
				ownerAgent := existing.agent
				gw.mu.Unlock()
				_ = wr.Write(ipcErrorEnv("routing-key-conflict", "already owned by "+ownerAgent))
				return
			}
			shim = &shimConn{
				conn:           conn,
				wr:             wr,
				agent:          env.Agent,
				index:          env.Index,
				routingKey:     env.RoutingKey,
				allowedUsers:   env.AllowedUsers,
				allowedRoles:   env.AllowedRoles,
				requireMention: env.RequireMention,
				inboundPrefix:  env.InboundPrefix,
			}
			gw.shims[env.RoutingKey] = shim
			gw.mu.Unlock()
			logf("subscribe agent=%s index=%d routingKey=%s", env.Agent, env.Index, env.RoutingKey)
			_ = wr.Write(ipcAckEnv(env.RoutingKey))
			gw.publishStatus(account, "")

		case "unsubscribe":
			if shim != nil && shim.routingKey == env.RoutingKey {
				gw.mu.Lock()
				delete(gw.shims, env.RoutingKey)
				gw.mu.Unlock()
				logf("unsubscribe routingKey=%s", env.RoutingKey)
				gw.publishStatus(account, "")
				shim = nil
			}

		case "send":
			if shim == nil {
				_ = wr.Write(ipcErrorEnv("not-subscribed", "send before subscribe"))
				continue
			}
			content := env.Content
			ids, err := sendDiscordMessage(gw.session, env.RoutingKey, content, nil, logf)
			if err != nil {
				logf("send err routingKey=%s: %v", env.RoutingKey, err)
				_ = wr.Write(ipcErrorEnv("send-failed", err.Error()))
				continue
			}
			gw.mu.Lock()
			gw.msgOut += len(ids)
			gw.mu.Unlock()
			// Acknowledge with an inbound-style envelope reusing Content for the id list.
			idJSON, _ := json.Marshal(ids)
			_ = wr.Write(ipcEnvelope{V: ipcVersion, Type: "send-ack", RoutingKey: env.RoutingKey, Content: string(idJSON)})

		case "list-channels":
			// Return JSON array of guilds/channels visible to the bot.
			channels := gw.listVisibleChannels()
			chJSON, _ := json.Marshal(channels)
			_ = wr.Write(ipcEnvelope{V: ipcVersion, Type: "list-channels-response", Content: string(chJSON)})

		default:
			_ = wr.Write(ipcErrorEnv("unknown-type", env.Type))
		}
	}
}

type channelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type int    `json:"type"`
}

func (gw *discordGateway) listVisibleChannels() []channelInfo {
	if gw.session == nil {
		return nil
	}
	guilds := gw.session.State.Guilds
	var out []channelInfo
	for _, g := range guilds {
		for _, ch := range g.Channels {
			if ch.Type == discordgo.ChannelTypeGuildText {
				out = append(out, channelInfo{ID: ch.ID, Name: ch.Name, Type: int(ch.Type)})
			}
		}
	}
	return out
}

// handleInbound processes a Discord MessageCreate event. Looks up the owning
// shim by channelId, applies allow-list and self-filter, and forwards.
func (gw *discordGateway) handleInbound(s *discordgo.Session, m *discordgo.MessageCreate, logf func(string, ...any)) {
	gw.mu.Lock()
	shim, ok := gw.shims[m.ChannelID]
	botID := gw.botID
	gw.mu.Unlock()

	if !ok {
		// No shim owns this channel — drop silently (counter tracked elsewhere).
		return
	}

	// Self-filter.
	if botID == "" && s.State != nil && s.State.User != nil {
		botID = s.State.User.ID
	}
	if botID != "" && m.Author != nil && m.Author.ID == botID {
		gw.mu.Lock()
		gw.dropped["self"]++
		gw.mu.Unlock()
		return
	}

	// Require-mention filter.
	if shim.requireMention {
		mentioned := false
		for _, u := range m.Mentions {
			if u.ID == botID {
				mentioned = true
				break
			}
		}
		if !mentioned {
			gw.mu.Lock()
			gw.dropped["no-mention"]++
			gw.mu.Unlock()
			return
		}
	}

	// Allow-list filter (same logic as v0, now at the gateway).
	if !gatewayAllowListPass(m, shim, botID) {
		gw.mu.Lock()
		gw.dropped["allow-list"]++
		gw.mu.Unlock()
		return
	}

	content := formatInboundContent(m, shim.inboundPrefix)
	sender := ""
	if m.Author != nil {
		sender = m.Author.Username
	}

	gw.mu.Lock()
	gw.msgIn++
	gw.mu.Unlock()

	logf("forward routingKey=%s sender=%s", m.ChannelID, sender)

	shimWr := shim.wr
	_ = shimWr.Write(ipcInboundEnv(m.ChannelID, sender, content))
}

// gatewayAllowListPass returns true if the message passes the shim's allow-list.
// OR semantics when both allowedUsers and allowedRoles are set.
func gatewayAllowListPass(m *discordgo.MessageCreate, shim *shimConn, botID string) bool {
	hasUsers := len(shim.allowedUsers) > 0
	hasRoles := len(shim.allowedRoles) > 0

	if !hasUsers && !hasRoles {
		return true
	}

	if m.Author == nil {
		return false
	}

	if hasUsers && !hasRoles {
		return contains(shim.allowedUsers, m.Author.ID)
	}
	if !hasUsers && hasRoles {
		if m.Member == nil {
			return false
		}
		for _, r := range m.Member.Roles {
			if contains(shim.allowedRoles, r) {
				return true
			}
		}
		return false
	}
	// Both lists set — OR.
	if contains(shim.allowedUsers, m.Author.ID) {
		return true
	}
	if m.Member != nil {
		for _, r := range m.Member.Roles {
			if contains(shim.allowedRoles, r) {
				return true
			}
		}
	}
	return false
}

// publishStatus writes the gateway's status.json atomically.
func (gw *discordGateway) publishStatus(account, botUsername string) {
	gw.mu.Lock()
	subs := make([]GatewaySubStatus, 0, len(gw.shims))
	for _, s := range gw.shims {
		subs = append(subs, GatewaySubStatus{Agent: s.agent, Index: s.index, RoutingKey: s.routingKey})
	}
	dropped := map[string]int{}
	for k, v := range gw.dropped {
		dropped[k] = v
	}
	st := GatewayStatus{
		Status:        gw.status,
		BotUserID:     gw.botID,
		BotUsername:   botUsername,
		MessagesIn:    gw.msgIn,
		MessagesOut:   gw.msgOut,
		Subscriptions: subs,
		DroppedBy:     dropped,
	}
	gw.mu.Unlock()
	_ = WriteGatewayStatus("discord", account, st)
}

// gatewayLogf returns a logger that writes to the gateway's log file.
func gatewayLogf(account string) func(string, ...any) {
	home, _ := os.UserHomeDir()
	dir := fmt.Sprintf("%s/.mesh/discord-accounts/%s", home, account)
	_ = os.MkdirAll(dir, 0o700)
	path := fmt.Sprintf("%s/gateway-%d.log", dir, os.Getpid())
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "[discord-gateway] "+format+"\n", args...)
		}
	}
	return func(format string, args ...any) {
		fmt.Fprintf(f, "["+time.Now().UTC().Format(time.RFC3339Nano)+"] "+format+"\n", args...)
	}
}
