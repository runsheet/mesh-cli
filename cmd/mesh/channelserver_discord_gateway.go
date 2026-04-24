package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sort"
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

// discordFilter holds the resolved per-message filter settings for a shim or
// an override entry. Used by both channel-mode and guild-mode routing.
type discordFilter struct {
	allowedUsers   []string
	allowedRoles   []string
	requireMention bool
	inboundPrefix  string
}

// shimConnOverride is a per-channel override stored on a guild-mode shimConn.
// It contains only the fields the subscriber explicitly set; nil means
// "inherit from guild default".
type shimConnOverride struct {
	requireMention *bool
	inboundPrefix  *string
	allowedUsers   []string // nil means "inherit"
	allowedRoles   []string // nil means "inherit"
}

// shimConn represents one connected shim.
//
// For channel-mode: routingKey == channelId; guildID is empty.
// For guild-mode: routingKey == guildId; channelID is empty; overrides is set.
type shimConn struct {
	conn             net.Conn
	wr               *ipcWriter
	agent            string
	index            int
	routingKey       string
	subscriptionMode string // "channel" or "guild"
	defaultFilter    discordFilter
	overrides        map[string]shimConnOverride // guild mode: keyed by channelId
}

// resolveFilter returns the effective discordFilter for a given channelId.
// For channel-mode, the filter is always the shimConn's defaultFilter.
// For guild-mode, overrides are merged on top of the default if present.
func (sc *shimConn) resolveFilter(channelID string) discordFilter {
	if sc.subscriptionMode != "guild" {
		return sc.defaultFilter
	}
	ov, ok := sc.overrides[channelID]
	if !ok {
		return sc.defaultFilter
	}
	f := sc.defaultFilter
	if ov.requireMention != nil {
		f.requireMention = *ov.requireMention
	}
	if ov.inboundPrefix != nil {
		f.inboundPrefix = *ov.inboundPrefix
	}
	if ov.allowedUsers != nil {
		f.allowedUsers = ov.allowedUsers
	}
	if ov.allowedRoles != nil {
		f.allowedRoles = ov.allowedRoles
	}
	return f
}

// discordGateway holds runtime state for one gateway process.
//
// Routing tables:
//   - channelIndex: channelId → shimConn. Holds channel-mode subscribers AND
//     guild-mode override channelIds (both stake exclusive ownership).
//   - guildIndex: guildId → shimConn. Holds guild-mode subscribers.
//
// Precedence for an inbound message (channelId C, guildId G):
//  1. channelIndex[C] → that shim owns it.
//  2. guildIndex[G]   → that shim owns it (guild-default).
//  3. Unrouted — increment drop counter.
type discordGateway struct {
	account string

	mu           sync.Mutex
	channelIndex map[string]*shimConn // channelId → shim (channel-mode + overrides)
	guildIndex   map[string]*shimConn // guildId → shim (guild-mode subscribers)
	botID        string
	status       string
	msgIn        int
	msgOut       int
	dropped      map[string]int // keyed by reason
	session      *discordgo.Session

	// typingUntil tracks the per-channel deadline for the "bot is typing…"
	// indicator. Set when an inbound is forwarded to a shim; cleared when
	// the corresponding outbound send arrives. A background goroutine
	// refreshes Discord's typing state every 5s until the deadline. Bounded
	// so a stuck agent doesn't leave the typing indicator on forever.
	typingUntil map[string]time.Time
}

const (
	typingRefreshInterval = 5 * time.Second
	typingMaxDuration     = 60 * time.Second
)

func newDiscordGateway(account string) *discordGateway {
	return &discordGateway{
		account:      account,
		channelIndex: map[string]*shimConn{},
		guildIndex:   map[string]*shimConn{},
		status:       "connecting",
		dropped:      map[string]int{},
		typingUntil:  map[string]time.Time{},
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
	// IntentsGuilds is required for discordgo to populate session.State.Guilds
	// and State.Channels; without it the bot stays connected but sees no
	// guilds, and list-channels returns empty even for a fully-invited bot.
	dg.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
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

	// Background typing-indicator refresher: Discord's typing state auto-
	// expires in ~10s, so we pulse it every 5s for channels awaiting a
	// reply. One goroutine per gateway.
	go gw.runTypingRefresher(ctx)

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
		if shim == nil {
			return
		}
		gw.mu.Lock()
		gw.removeShimLocked(shim)
		gw.mu.Unlock()
		logf("shim disconnected agent=%s index=%d routingKey=%s", shim.agent, shim.index, shim.routingKey)
		gw.publishStatus(account, "")
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
			mode := env.SubscriptionMode
			if mode == "" {
				mode = "channel"
			}

			newShim, conflictErr := gw.buildAndRegisterShim(conn, wr, env, mode)
			if conflictErr != "" {
				_ = wr.Write(ipcErrorEnv("routing-key-conflict", conflictErr))
				return
			}
			shim = newShim
			logf("subscribe agent=%s index=%d routingKey=%s mode=%s", env.Agent, env.Index, env.RoutingKey, mode)
			_ = wr.Write(ipcAckEnv(env.RoutingKey))
			gw.publishStatus(account, "")

		case "unsubscribe":
			if shim != nil && shim.routingKey == env.RoutingKey {
				gw.mu.Lock()
				gw.removeShimLocked(shim)
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
			// env.ChannelID is the explicit target channel. For channel-mode
			// the shim always sends the channelId directly. For guild-mode it
			// may differ from the guildId routingKey.
			targetChannelID := env.ChannelID
			if targetChannelID == "" {
				// Fallback: treat routingKey as channel (channel-mode shims using
				// old ipcSendEnv that still set RoutingKey).
				targetChannelID = env.RoutingKey
			}
			ids, err := sendDiscordMessage(gw.session, targetChannelID, env.Content, nil, logf)
			// Outbound landed — clear typing indicator for that channel so
			// we stop pulsing "bot is typing…" now that the real message
			// is visible. Safe to call even on send error (typing auto-
			// expires on Discord's side anyway).
			gw.stopTyping(targetChannelID)
			if err != nil {
				logf("send err channelId=%s: %v", targetChannelID, err)
				_ = wr.Write(ipcErrorEnv("send-failed", err.Error()))
				continue
			}
			gw.mu.Lock()
			gw.msgOut += len(ids)
			gw.mu.Unlock()
			idJSON, _ := json.Marshal(ids)
			_ = wr.Write(ipcEnvelope{V: ipcVersion, Type: "send-ack", ChannelID: targetChannelID, Content: string(idJSON)})

		case "list-channels":
			channels := gw.listVisibleChannels()
			chJSON, _ := json.Marshal(channels)
			_ = wr.Write(ipcEnvelope{V: ipcVersion, Type: "list-channels-response", Content: string(chJSON)})

		default:
			_ = wr.Write(ipcErrorEnv("unknown-type", env.Type))
		}
	}
}

// buildAndRegisterShim validates the subscribe envelope against the current
// routing tables, builds a shimConn, and registers it. Returns the new shim
// and an empty conflict string on success; returns nil and a non-empty conflict
// detail on conflict. Must be called without gw.mu held.
func (gw *discordGateway) buildAndRegisterShim(conn net.Conn, wr *ipcWriter, env ipcEnvelope, mode string) (*shimConn, string) {
	gw.mu.Lock()
	defer gw.mu.Unlock()

	switch mode {
	case "channel":
		if existing, taken := gw.channelIndex[env.RoutingKey]; taken {
			return nil, "channelId " + env.RoutingKey + " already owned by " + existing.agent
		}
		s := &shimConn{
			conn:             conn,
			wr:               wr,
			agent:            env.Agent,
			index:            env.Index,
			routingKey:       env.RoutingKey,
			subscriptionMode: "channel",
			defaultFilter: discordFilter{
				allowedUsers:   env.AllowedUsers,
				allowedRoles:   env.AllowedRoles,
				requireMention: env.RequireMention,
				inboundPrefix:  env.InboundPrefix,
			},
		}
		gw.channelIndex[env.RoutingKey] = s
		return s, ""

	case "guild":
		if existing, taken := gw.guildIndex[env.RoutingKey]; taken {
			return nil, "guildId " + env.RoutingKey + " already owned by " + existing.agent
		}
		// Check every override channelId against the channel-index.
		for _, ov := range env.ChannelOverrides {
			if existing, taken := gw.channelIndex[ov.ChannelID]; taken {
				return nil, "channelId " + ov.ChannelID + " already owned by " + existing.agent
			}
		}
		overrides := make(map[string]shimConnOverride, len(env.ChannelOverrides))
		for _, ov := range env.ChannelOverrides {
			sov := shimConnOverride{
				requireMention: ov.RequireMention,
				inboundPrefix:  ov.InboundPrefix,
			}
			if ov.AllowedUsers != nil {
				sov.allowedUsers = ov.AllowedUsers
			}
			if ov.AllowedRoles != nil {
				sov.allowedRoles = ov.AllowedRoles
			}
			overrides[ov.ChannelID] = sov
		}
		s := &shimConn{
			conn:             conn,
			wr:               wr,
			agent:            env.Agent,
			index:            env.Index,
			routingKey:       env.RoutingKey,
			subscriptionMode: "guild",
			defaultFilter: discordFilter{
				allowedUsers:   env.AllowedUsers,
				allowedRoles:   env.AllowedRoles,
				requireMention: env.RequireMention,
				inboundPrefix:  env.InboundPrefix,
			},
			overrides: overrides,
		}
		// Register the guild in guildIndex.
		gw.guildIndex[env.RoutingKey] = s
		// Register each override channelId in channelIndex so they stake
		// exclusive ownership and benefit from O(1) lookup.
		for channelID := range overrides {
			gw.channelIndex[channelID] = s
		}
		return s, ""

	default:
		return nil, "unknown subscriptionMode: " + mode
	}
}

// removeShimLocked removes a shim from both indexes. Must be called with
// gw.mu held.
func (gw *discordGateway) removeShimLocked(s *shimConn) {
	switch s.subscriptionMode {
	case "guild":
		if gw.guildIndex[s.routingKey] == s {
			delete(gw.guildIndex, s.routingKey)
		}
		// Remove override channelId entries.
		for channelID, indexed := range gw.channelIndex {
			if indexed == s {
				delete(gw.channelIndex, channelID)
			}
		}
	default:
		if gw.channelIndex[s.routingKey] == s {
			delete(gw.channelIndex, s.routingKey)
		}
	}
}

type channelInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    int    `json:"type"`
	GuildID string `json:"guildId,omitempty"`
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
				out = append(out, channelInfo{ID: ch.ID, Name: ch.Name, Type: int(ch.Type), GuildID: g.ID})
			}
		}
	}
	return out
}

// handleInbound processes a Discord MessageCreate event using the two-level
// routing precedence:
//  1. channelIndex[m.ChannelID] → that shim owns the message.
//  2. guildIndex[m.GuildID]     → guild-default subscriber.
//  3. Unrouted — increment drop counter.
//
// Filter settings are resolved per-channel (overrides merged on top of guild
// defaults for guild-mode).
func (gw *discordGateway) handleInbound(s *discordgo.Session, m *discordgo.MessageCreate, logf func(string, ...any)) {
	gw.mu.Lock()
	shim, routingKey := gw.resolveOwner(m.ChannelID, m.GuildID)
	botID := gw.botID
	gw.mu.Unlock()

	if shim == nil {
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

	// Resolve effective filter settings for this specific channel.
	filter := shim.resolveFilter(m.ChannelID)

	// Require-mention filter.
	if filter.requireMention {
		mentioned := false
		// User mention: <@botUserID>.
		for _, u := range m.Mentions {
			if u.ID == botID {
				mentioned = true
				break
			}
		}
		// Role mention: <@&roleID> where the role is one the bot holds in the
		// guild. Covers the common case where a user types @botname and
		// autocomplete picks a role named after the bot rather than the bot
		// user itself.
		if !mentioned && len(m.MentionRoles) > 0 {
			botRoles := gw.botRolesInGuild(m.GuildID)
			for _, rid := range m.MentionRoles {
				if botRoles[rid] {
					mentioned = true
					break
				}
			}
		}
		if !mentioned {
			gw.mu.Lock()
			gw.dropped["no-mention"]++
			gw.mu.Unlock()
			logf("drop no-mention channelId=%s from=%s content=%q userMentions=%d roleMentions=%d",
				m.ChannelID, m.Author.Username, m.Content, len(m.Mentions), len(m.MentionRoles))
			return
		}
	}

	// Allow-list filter.
	if !gatewayAllowListPassFilter(m, filter) {
		gw.mu.Lock()
		gw.dropped["allow-list"]++
		gw.mu.Unlock()
		return
	}

	// Send raw content. The shim now emits notifications/claude/channel with
	// sender metadata in a structured meta block, so the inboundPrefix
	// formatting (which was a workaround for the older log-notification
	// approach) is no longer needed.
	content := m.Content
	sender := ""
	if m.Author != nil {
		sender = m.Author.Username
	}

	gw.mu.Lock()
	gw.msgIn++
	gw.mu.Unlock()

	logf("forward routingKey=%s channelId=%s sender=%s", routingKey, m.ChannelID, sender)

	shimWr := shim.wr
	var userID string
	if m.Author != nil {
		userID = m.Author.ID
	}
	_ = shimWr.Write(ipcInboundEnvFull(
		routingKey,
		m.ChannelID,
		sender,
		m.ID,
		userID,
		m.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z"),
		content,
	))

	// Start "bot is typing…" for this channel. The refresh goroutine keeps
	// it alive until the matching outbound send arrives or the max-duration
	// deadline hits.
	gw.startTyping(m.ChannelID)
}

// startTyping arms the typing indicator for a channel. Safe to call when
// already armed — it just extends the deadline.
func (gw *discordGateway) startTyping(channelID string) {
	if channelID == "" {
		return
	}
	gw.mu.Lock()
	gw.typingUntil[channelID] = time.Now().Add(typingMaxDuration)
	gw.mu.Unlock()
	// Fire an immediate typing pulse so Discord starts showing the
	// indicator right away; the refresh loop takes over from there.
	if gw.session != nil {
		_ = gw.session.ChannelTyping(channelID)
	}
}

// stopTyping clears the typing indicator for a channel. Called when an
// outbound send lands (the actual message replaces the typing indicator
// naturally, but we still drop the refresh goroutine's interest).
func (gw *discordGateway) stopTyping(channelID string) {
	if channelID == "" {
		return
	}
	gw.mu.Lock()
	delete(gw.typingUntil, channelID)
	gw.mu.Unlock()
}

// runTypingRefresher is the background goroutine that pulses Discord's
// typing state every typingRefreshInterval for each channel still within
// its deadline. Exits when ctx is cancelled.
func (gw *discordGateway) runTypingRefresher(ctx context.Context) {
	t := time.NewTicker(typingRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			gw.mu.Lock()
			var active []string
			for cid, until := range gw.typingUntil {
				if now.Before(until) {
					active = append(active, cid)
				} else {
					delete(gw.typingUntil, cid)
				}
			}
			gw.mu.Unlock()
			if gw.session == nil {
				continue
			}
			for _, cid := range active {
				_ = gw.session.ChannelTyping(cid)
			}
		}
	}
}

// botRolesInGuild returns the set of role IDs the bot holds in the given
// guild. Used by the require-mention filter to accept role mentions that
// resolve to a role the bot is in (e.g. users typing @larry and picking a
// bot-named role instead of the bot user).
func (gw *discordGateway) botRolesInGuild(guildID string) map[string]bool {
	if gw.session == nil || guildID == "" {
		return nil
	}
	selfID := ""
	if gw.session.State != nil && gw.session.State.User != nil {
		selfID = gw.session.State.User.ID
	}
	if selfID == "" {
		return nil
	}
	member, err := gw.session.State.Member(guildID, selfID)
	if err != nil || member == nil {
		return nil
	}
	out := make(map[string]bool, len(member.Roles))
	for _, r := range member.Roles {
		out[r] = true
	}
	return out
}

// resolveOwner implements the routing precedence. Returns the owning shim and
// the routingKey it was registered under. Must be called with gw.mu held.
func (gw *discordGateway) resolveOwner(channelID, guildID string) (*shimConn, string) {
	if s, ok := gw.channelIndex[channelID]; ok {
		return s, s.routingKey
	}
	if guildID != "" {
		if s, ok := gw.guildIndex[guildID]; ok {
			return s, guildID
		}
	}
	return nil, ""
}

// gatewayAllowListPassFilter returns true if the message passes the filter.
// OR semantics when both allowedUsers and allowedRoles are set.
func gatewayAllowListPassFilter(m *discordgo.MessageCreate, f discordFilter) bool {
	hasUsers := len(f.allowedUsers) > 0
	hasRoles := len(f.allowedRoles) > 0

	if !hasUsers && !hasRoles {
		return true
	}

	if m.Author == nil {
		return false
	}

	if hasUsers && !hasRoles {
		return contains(f.allowedUsers, m.Author.ID)
	}
	if !hasUsers && hasRoles {
		if m.Member == nil {
			return false
		}
		for _, r := range m.Member.Roles {
			if contains(f.allowedRoles, r) {
				return true
			}
		}
		return false
	}
	// Both lists set — OR.
	if contains(f.allowedUsers, m.Author.ID) {
		return true
	}
	if m.Member != nil {
		for _, r := range m.Member.Roles {
			if contains(f.allowedRoles, r) {
				return true
			}
		}
	}
	return false
}

// publishStatus writes the gateway's status.json atomically.
func (gw *discordGateway) publishStatus(account, botUsername string) {
	gw.mu.Lock()
	// Collect unique shims: guild-mode shims appear in both channelIndex and
	// guildIndex; emit each once, keyed by routingKey.
	seen := map[string]bool{}
	subs := make([]GatewaySubStatus, 0)
	for _, s := range gw.guildIndex {
		if seen[s.routingKey] {
			continue
		}
		seen[s.routingKey] = true
		overrideIDs := make([]string, 0, len(s.overrides))
		for cid := range s.overrides {
			overrideIDs = append(overrideIDs, cid)
		}
		sort.Strings(overrideIDs)
		subs = append(subs, GatewaySubStatus{
			Agent:            s.agent,
			Index:            s.index,
			RoutingKey:       s.routingKey,
			SubscriptionMode: "guild",
			OverrideCount:    len(s.overrides),
			Overrides:        overrideIDs,
		})
	}
	for _, s := range gw.channelIndex {
		if s.subscriptionMode == "guild" {
			// Already counted above via guildIndex.
			continue
		}
		if seen[s.routingKey] {
			continue
		}
		seen[s.routingKey] = true
		subs = append(subs, GatewaySubStatus{
			Agent:            s.agent,
			Index:            s.index,
			RoutingKey:       s.routingKey,
			SubscriptionMode: "channel",
		})
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
