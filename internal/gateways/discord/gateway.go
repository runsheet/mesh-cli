package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"projectmesh.io/mesh-cli/internal/accounts"
	"projectmesh.io/mesh-cli/internal/ipc"
	"projectmesh.io/mesh-cli/internal/media"
	"projectmesh.io/mesh-cli/internal/paths"
)

// filter holds the resolved per-message filter settings for a shim or
// an override entry. Used by both channel-mode and guild-mode routing.
type filter struct {
	allowedUsers   []string
	allowedRoles   []string
	requireMention bool
	inboundPrefix  string
}

// shimOverride is a per-channel override stored on a guild-mode shimConn.
// It contains only the fields the subscriber explicitly set; nil means
// "inherit from guild default".
type shimOverride struct {
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
	writeMu          sync.Mutex // guards wr; concurrent attachment downloads write concurrently
	wr               *ipc.Writer
	agent            string
	index            int
	routingKey       string
	subscriptionMode string // "channel" or "guild"
	defaultFilter    filter
	overrides        map[string]shimOverride // guild mode: keyed by channelId
}

// resolveFilter returns the effective filter for a given channelId.
// For channel-mode, the filter is always the shimConn's defaultFilter.
// For guild-mode, overrides are merged on top of the default if present.
func (sc *shimConn) resolveFilter(channelID string) filter {
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

// channelInfo is the wire shape for a visible Discord text channel.
type channelInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    int    `json:"type"`
	GuildID string `json:"guildId,omitempty"`
}

const (
	typingRefreshInterval = 5 * time.Second
	typingMaxDuration     = 60 * time.Second
)

// Gateway holds runtime state for one Discord gateway process.
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
type Gateway struct {
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
	// indicator.
	typingUntil map[string]time.Time
}

func newGateway(account string) *Gateway {
	return &Gateway{
		account:      account,
		channelIndex: map[string]*shimConn{},
		guildIndex:   map[string]*shimConn{},
		status:       "connecting",
		dropped:      map[string]int{},
		typingUntil:  map[string]time.Time{},
	}
}

// Run is the entry point for the Discord gateway process.
func Run(account string) error {
	token, err := accounts.LoadDiscordToken(account)
	if err != nil {
		return fmt.Errorf("load token: %w", err)
	}
	token = strings.TrimSpace(token)

	logf := gatewayLogf(account)
	logf("discord gateway start account=%s pid=%d", account, os.Getpid())

	gw := newGateway(account)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		logf("signal %s received, shutting down", sig)
		cancel()
	}()

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
		meta, _ := accounts.LoadDiscordAccountMeta(account)
		meta.BotUserID = r.User.ID
		_ = accounts.UpdateDiscordAccountMeta(account, meta)
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

	sockPath, err := paths.DiscordGatewaySockPath(account)
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

	PruneMedia(account)

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

	go gw.runTypingRefresher(ctx)

	<-ctx.Done()
	logf("discord gateway shutting down")
	return nil
}

// handleShim serves one shim connection until the connection closes or ctx
// is cancelled.
func (gw *Gateway) handleShim(ctx context.Context, conn net.Conn, account string, logf func(string, ...any)) {
	defer conn.Close()
	rd := ipc.NewReader(conn)
	wr := ipc.NewWriter(conn)
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
				_ = wr.Write(ipc.ErrorEnv("bad-subscribe", "routingKey is required"))
				return
			}
			mode := env.SubscriptionMode
			if mode == "" {
				mode = "channel"
			}

			newShim, conflictErr := gw.buildAndRegisterShim(conn, wr, env, mode)
			if conflictErr != "" {
				_ = wr.Write(ipc.ErrorEnv("routing-key-conflict", conflictErr))
				return
			}
			shim = newShim
			logf("subscribe agent=%s index=%d routingKey=%s mode=%s", env.Agent, env.Index, env.RoutingKey, mode)
			_ = wr.Write(ipc.AckEnv(env.RoutingKey))
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
				_ = wr.Write(ipc.ErrorEnv("not-subscribed", "send before subscribe"))
				continue
			}
			targetChannelID := env.ChannelID
			if targetChannelID == "" {
				targetChannelID = env.RoutingKey
			}
			var (
				ids    []string
				sendErr error
			)
			if len(env.Attachments) > 0 {
				ids, sendErr = SendMessageWithAttachment(gw.session, targetChannelID, env.Content, env.Attachments[0], logf)
			} else {
				ids, sendErr = SendMessage(gw.session, targetChannelID, env.Content, nil, logf)
			}
			gw.stopTyping(targetChannelID)
			if sendErr != nil {
				logf("send err channelId=%s: %v", targetChannelID, sendErr)
				_ = wr.Write(ipc.ErrorEnv("send-failed", sendErr.Error()))
				continue
			}
			gw.mu.Lock()
			gw.msgOut += len(ids)
			gw.mu.Unlock()
			idJSON, _ := json.Marshal(ids)
			_ = wr.Write(ipc.Envelope{V: ipc.Version, Type: "send-ack", ChannelID: targetChannelID, Content: string(idJSON)})

		case "list-channels":
			channels := gw.listVisibleChannels()
			chJSON, _ := json.Marshal(channels)
			_ = wr.Write(ipc.Envelope{V: ipc.Version, Type: "list-channels-response", Content: string(chJSON)})

		case "dm-open":
			if gw.session == nil {
				_ = wr.Write(ipc.ErrorEnv("not-connected", "discord session not open"))
				continue
			}
			ch, err := gw.session.UserChannelCreate(env.UserID)
			if err != nil {
				_ = wr.Write(ipc.ErrorEnv("dm-open-failed", err.Error()))
				continue
			}
			_ = wr.Write(ipc.Envelope{V: ipc.Version, Type: "dm-open-ack", ChannelID: ch.ID})

		default:
			_ = wr.Write(ipc.ErrorEnv("unknown-type", env.Type))
		}
	}
}

// buildAndRegisterShim validates the subscribe envelope against the current
// routing tables, builds a shimConn, and registers it. Returns the new shim
// and an empty conflict string on success; returns nil and a non-empty conflict
// detail on conflict. Must be called without gw.mu held.
func (gw *Gateway) buildAndRegisterShim(conn net.Conn, wr *ipc.Writer, env ipc.Envelope, mode string) (*shimConn, string) {
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
			defaultFilter: filter{
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
		for _, ov := range env.ChannelOverrides {
			if existing, taken := gw.channelIndex[ov.ChannelID]; taken {
				return nil, "channelId " + ov.ChannelID + " already owned by " + existing.agent
			}
		}
		overrides := make(map[string]shimOverride, len(env.ChannelOverrides))
		for _, ov := range env.ChannelOverrides {
			sov := shimOverride{
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
			defaultFilter: filter{
				allowedUsers:   env.AllowedUsers,
				allowedRoles:   env.AllowedRoles,
				requireMention: env.RequireMention,
				inboundPrefix:  env.InboundPrefix,
			},
			overrides: overrides,
		}
		gw.guildIndex[env.RoutingKey] = s
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
func (gw *Gateway) removeShimLocked(s *shimConn) {
	switch s.subscriptionMode {
	case "guild":
		if gw.guildIndex[s.routingKey] == s {
			delete(gw.guildIndex, s.routingKey)
		}
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

func (gw *Gateway) listVisibleChannels() []channelInfo {
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
// routing precedence.
func (gw *Gateway) handleInbound(s *discordgo.Session, m *discordgo.MessageCreate, logf func(string, ...any)) {
	gw.mu.Lock()
	shim, routingKey := gw.resolveOwner(m.ChannelID, m.GuildID)
	botID := gw.botID
	gw.mu.Unlock()

	if shim == nil {
		return
	}

	if botID == "" && s.State != nil && s.State.User != nil {
		botID = s.State.User.ID
	}
	if botID != "" && m.Author != nil && m.Author.ID == botID {
		gw.mu.Lock()
		gw.dropped["self"]++
		gw.mu.Unlock()
		return
	}

	f := shim.resolveFilter(m.ChannelID)

	if f.requireMention {
		mentioned := false
		for _, u := range m.Mentions {
			if u.ID == botID {
				mentioned = true
				break
			}
		}
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

	if !allowListPassFilter(m, f) {
		gw.mu.Lock()
		gw.dropped["allow-list"]++
		gw.mu.Unlock()
		return
	}

	content := m.Content
	sender := ""
	if m.Author != nil {
		sender = m.Author.Username
	}

	gw.mu.Lock()
	gw.msgIn++
	gw.mu.Unlock()

	logf("forward routingKey=%s channelId=%s sender=%s attachments=%d", routingKey, m.ChannelID, sender, len(m.Attachments))

	var userID string
	if m.Author != nil {
		userID = m.Author.ID
	}
	attachments := gw.downloadAttachments(m, logf)

	shim.writeMu.Lock()
	_ = shim.wr.Write(ipc.InboundEnvFull(
		routingKey,
		m.ChannelID,
		sender,
		m.ID,
		userID,
		m.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z"),
		content,
		attachments,
	))
	shim.writeMu.Unlock()

	gw.startTyping(m.ChannelID)
}

func (gw *Gateway) startTyping(channelID string) {
	if channelID == "" {
		return
	}
	gw.mu.Lock()
	gw.typingUntil[channelID] = time.Now().Add(typingMaxDuration)
	gw.mu.Unlock()
	if gw.session != nil {
		_ = gw.session.ChannelTyping(channelID)
	}
}

func (gw *Gateway) stopTyping(channelID string) {
	if channelID == "" {
		return
	}
	gw.mu.Lock()
	delete(gw.typingUntil, channelID)
	gw.mu.Unlock()
}

func (gw *Gateway) runTypingRefresher(ctx context.Context) {
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

func (gw *Gateway) botRolesInGuild(guildID string) map[string]bool {
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

// resolveOwner implements the routing precedence. Must be called with gw.mu held.
func (gw *Gateway) resolveOwner(channelID, guildID string) (*shimConn, string) {
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

// allowListPassFilter returns true if the message passes the filter.
// OR semantics when both allowedUsers and allowedRoles are set.
func allowListPassFilter(m *discordgo.MessageCreate, f filter) bool {
	hasUsers := len(f.allowedUsers) > 0
	hasRoles := len(f.allowedRoles) > 0

	if !hasUsers && !hasRoles {
		return true
	}

	if m.Author == nil {
		return false
	}

	if hasUsers && !hasRoles {
		return Contains(f.allowedUsers, m.Author.ID)
	}
	if !hasUsers && hasRoles {
		if m.Member == nil {
			return false
		}
		for _, r := range m.Member.Roles {
			if Contains(f.allowedRoles, r) {
				return true
			}
		}
		return false
	}
	if Contains(f.allowedUsers, m.Author.ID) {
		return true
	}
	if m.Member != nil {
		for _, r := range m.Member.Roles {
			if Contains(f.allowedRoles, r) {
				return true
			}
		}
	}
	return false
}

// downloadAttachments downloads allowlisted image attachments from a Discord
// message concurrently with a 30s collective timeout.
func (gw *Gateway) downloadAttachments(m *discordgo.MessageCreate, logf func(string, ...any)) []ipc.Attachment {
	type indexedResult struct {
		idx int
		att ipc.Attachment
	}

	type candidate struct {
		idx  int
		att  *discordgo.MessageAttachment
		mime string
	}
	var candidates []candidate
	for i, a := range m.Attachments {
		mime := a.ContentType
		if semi := strings.IndexByte(mime, ';'); semi >= 0 {
			mime = strings.TrimSpace(mime[:semi])
		}
		if media.ImageMIMEs[mime] {
			candidates = append(candidates, candidate{idx: i, att: a, mime: mime})
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	httpClient := &http.Client{}
	resultCh := make(chan indexedResult, len(candidates))

	for _, c := range candidates {
		c := c
		go func() {
			data, failReason := DownloadAttachment(c.att, httpClient)
			if failReason != "" {
				logf("download err msgId=%s idx=%d mime=%s: %s", m.ID, c.idx, c.mime, failReason)
				resultCh <- indexedResult{c.idx, ipc.Attachment{Type: "image", MIMEType: c.mime, FailReason: failReason}}
				return
			}
			if ctx.Err() != nil {
				resultCh <- indexedResult{c.idx, ipc.Attachment{Type: "image", MIMEType: c.mime, FailReason: "download timeout after 30s"}}
				return
			}
			path, failReason := WriteAttachment(gw.account, m.ID, c.idx, c.mime, data)
			if failReason != "" {
				logf("write err msgId=%s idx=%d: %s", m.ID, c.idx, failReason)
				resultCh <- indexedResult{c.idx, ipc.Attachment{Type: "image", MIMEType: c.mime, FailReason: failReason}}
				return
			}
			logf("download ok path=%s", path)
			resultCh <- indexedResult{c.idx, ipc.Attachment{Type: "image", Path: path, MIMEType: c.mime}}
		}()
	}

	byIdx := make(map[int]ipc.Attachment, len(candidates))
	for range candidates {
		select {
		case r := <-resultCh:
			byIdx[r.idx] = r.att
		case <-ctx.Done():
			for _, c := range candidates {
				if _, done := byIdx[c.idx]; !done {
					byIdx[c.idx] = ipc.Attachment{Type: "image", MIMEType: c.mime, FailReason: "download timeout after 30s"}
				}
			}
			goto assemble
		}
	}

assemble:
	out := make([]ipc.Attachment, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, byIdx[c.idx])
	}
	PruneMedia(gw.account)
	return out
}

// publishStatus writes the gateway's status.json atomically.
func (gw *Gateway) publishStatus(account, botUsername string) {
	gw.mu.Lock()
	seen := map[string]bool{}
	subs := make([]accounts.GatewaySubStatus, 0)
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
		subs = append(subs, accounts.GatewaySubStatus{
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
			continue
		}
		if seen[s.routingKey] {
			continue
		}
		seen[s.routingKey] = true
		subs = append(subs, accounts.GatewaySubStatus{
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
	st := accounts.GatewayStatus{
		Status:        gw.status,
		BotUserID:     gw.botID,
		BotUsername:   botUsername,
		MessagesIn:    gw.msgIn,
		MessagesOut:   gw.msgOut,
		Subscriptions: subs,
		DroppedBy:     dropped,
	}
	gw.mu.Unlock()
	_ = accounts.WriteGatewayStatus("discord", account, st)
}

// gatewayLogf returns a logger that writes to the gateway's log file.
func gatewayLogf(account string) func(string, ...any) {
	dir, _ := paths.PlatformAccountDir("discord", account)
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
