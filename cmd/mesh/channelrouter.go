package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MeshRouterSockPath returns the path of the per-agent router socket.
// The router (daemon-side) listens here; the mesh-shim connects to it.
func MeshRouterSockPath(agentName string) (string, error) {
	dir, err := agentWorkdir(agentName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mesh.sock"), nil
}

// agentRouter is the daemon-side multiplexer for one agent. It:
//  1. Listens on ~/.mesh/agents/<name>/mesh.sock (the shim connects here).
//  2. Accepts exactly one shim connection at a time.
//  3. For each inbound envelope from a gateway, stamps Source and forwards to
//     the shim.
//  4. For each outbound envelope from the shim, strips Target, routes to the
//     correct gateway socket, and forwards the (re-tagged) envelope.
//
// The router is intentionally thin — it does not own the gateway connections
// or gateway state; it dials each gateway on first outbound/subscribe and
// reconnects when needed. The gateway subprocess is managed by ChannelManager.
type agentRouter struct {
	agentName string
	channels  []ChannelSpec

	mu       sync.Mutex
	shimConn net.Conn      // single connected shim (nil if none)
	shimWr   *ipcWriter
	gwConns  map[string]*routerGWConn // key: "platform:account"
}

type routerGWConn struct {
	platform string
	account  string
	conn     net.Conn
	rd       *ipcReader
	wr       *ipcWriter
}

// ChannelRouter manages one agentRouter per agent. The daemon creates a
// ChannelRouter and calls Sync on every agent-set change.
type ChannelRouter struct {
	mu      sync.Mutex
	routers map[string]*runningRouter // key: agentName
	exe     string
}

type runningRouter struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewChannelRouter creates an empty ChannelRouter.
func NewChannelRouter() *ChannelRouter {
	return &ChannelRouter{routers: map[string]*runningRouter{}}
}

// Sync reconciles running routers against the provided agent list.
// Routers for agents that have been removed are stopped; new agents get a
// fresh router goroutine.
func (cr *ChannelRouter) Sync(agents []*AgentDef) {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	desired := map[string]*AgentDef{}
	for _, a := range agents {
		if len(a.Channels) > 0 {
			desired[a.Name] = a
		}
	}

	// Start new.
	for name, a := range desired {
		if _, running := cr.routers[name]; !running {
			cr.startRouterLocked(name, a.Channels)
		}
	}

	// Stop removed.
	for name, rr := range cr.routers {
		if _, want := desired[name]; !want {
			rr.cancel()
			<-rr.done
			delete(cr.routers, name)
		}
	}
}

// StopAll stops all running routers. Called on daemon shutdown.
func (cr *ChannelRouter) StopAll() {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	for name, rr := range cr.routers {
		rr.cancel()
		<-rr.done
		delete(cr.routers, name)
	}
}

func (cr *ChannelRouter) startRouterLocked(agentName string, channels []ChannelSpec) {
	ctx, cancel := context.WithCancel(context.Background())
	rr := &runningRouter{cancel: cancel, done: make(chan struct{})}
	cr.routers[agentName] = rr
	go func() {
		defer close(rr.done)
		runAgentRouter(ctx, agentName, channels)
	}()
}

// runAgentRouter is the goroutine that owns one agent's mesh.sock listener.
// It loops: accept a shim connection, serve it until it closes, repeat.
func runAgentRouter(ctx context.Context, agentName string, channels []ChannelSpec) {
	logf := routerLogf(agentName)
	sockPath, err := MeshRouterSockPath(agentName)
	if err != nil {
		logf("router sock path: %v", err)
		return
	}
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		logf("router listen %s: %v", sockPath, err)
		return
	}
	defer func() {
		ln.Close()
		_ = os.Remove(sockPath)
	}()
	if err := os.Chmod(sockPath, 0o600); err != nil {
		logf("chmod mesh.sock: %v", err)
	}
	logf("router listening agent=%s sock=%s", agentName, sockPath)

	// Also write a subs file so the shim can GET its subscription list without
	// a separate RPC round-trip: the shim reads it on startup via
	// readRouterSubscriptions. We write it once here; it's regenerated on
	// daemon restart / sync.
	writeRouterSubsFile(agentName, channels)

	for {
		// Use a channel to detect ctx cancellation while blocked in Accept.
		connCh := make(chan net.Conn, 1)
		errCh := make(chan error, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				errCh <- err
			} else {
				connCh <- c
			}
		}()
		select {
		case <-ctx.Done():
			ln.Close()
			return
		case err := <-errCh:
			if ctx.Err() != nil {
				return
			}
			logf("accept error: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		case conn := <-connCh:
			r := &agentRouter{
				agentName: agentName,
				channels:  channels,
				gwConns:   map[string]*routerGWConn{},
			}
			r.serveShim(ctx, conn, logf)
		}
	}
}

// serveShim is the per-shim-connection handler. It:
//  - reads the subscribe envelope from the shim and propagates subscriptions
//    to the relevant gateways.
//  - multiplexes inbound messages from gateways (with Source stamped) to the
//    shim.
//  - routes outbound send envelopes from the shim to the correct gateway.
func (r *agentRouter) serveShim(ctx context.Context, conn net.Conn, logf func(string, ...any)) {
	defer conn.Close()

	r.mu.Lock()
	r.shimConn = conn
	r.shimWr = newIPCWriter(conn)
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.shimConn = nil
		r.shimWr = nil
		// Close all gateway connections when the shim disconnects.
		for _, gw := range r.gwConns {
			gw.conn.Close()
		}
		r.gwConns = map[string]*routerGWConn{}
		r.mu.Unlock()
	}()

	rd := newIPCReader(conn)
	logf("shim connected agent=%s", r.agentName)

	for {
		if ctx.Err() != nil {
			return
		}
		env, err := rd.Read()
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				logf("shim read err: %v", err)
			}
			return
		}

		switch env.Type {
		case "subscribe":
			r.handleShimSubscribe(ctx, env, logf)
		case "unsubscribe":
			r.handleShimUnsubscribe(env, logf)
		case "send":
			r.handleShimSend(env, logf)
		default:
			logf("unknown shim envelope type=%s", env.Type)
		}
	}
}

// handleShimSubscribe fans the subscribe out to every discord gateway whose
// account is referenced in the agent's channel list. For slice 1, that means
// all discord channels. Each gateway gets its own connection; the router
// goroutine for each gateway pumps inbound messages back to the shim with
// Source stamped.
func (r *agentRouter) handleShimSubscribe(ctx context.Context, env ipcEnvelope, logf func(string, ...any)) {
	logf("subscribe from shim agent=%s", r.agentName)

	// Propagate subscriptions to each discord gateway referenced by the agent.
	for _, ch := range r.channels {
		switch ch.Type {
		case "discord":
			r.subscribeDiscordGateway(ctx, ch, env, logf)
		}
	}

	// Send ack back to shim with a synthetic routing key.
	r.mu.Lock()
	wr := r.shimWr
	r.mu.Unlock()
	if wr != nil {
		_ = wr.Write(ipcAckEnv("agent:" + r.agentName))
	}
}

func (r *agentRouter) subscribeDiscordGateway(ctx context.Context, ch ChannelSpec, shimEnv ipcEnvelope, logf func(string, ...any)) {
	dcc := parseDiscordChannelConfig(ch.Config)
	if dcc.Account == "" {
		logf("discord channel missing account")
		return
	}
	gwKey := gatewayKey("discord", dcc.Account)

	r.mu.Lock()
	_, already := r.gwConns[gwKey]
	r.mu.Unlock()
	if already {
		return
	}

	sockPath, err := DiscordGatewaySockPath(dcc.Account)
	if err != nil {
		logf("discord gateway sock path: %v", err)
		return
	}

	// Retry connecting to the gateway socket for up to 30s — the gateway
	// subprocess may still be starting.
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
			logf("discord gateway dial deadline: %v", err)
			return
		}
		logf("discord gateway dial retry: %v", err)
		time.Sleep(200 * time.Millisecond)
	}

	// Build the subscribe envelope for the discord gateway. Reuse the
	// subscription mode and routing key from the channel config (not from the
	// shim envelope, which carries the agent-level routing key).
	var subEnv ipcEnvelope
	switch dcc.SubscriptionMode {
	case "guild":
		overrides := make([]ipcChannelOverride, 0, len(dcc.ChannelOverrides))
		for _, ov := range dcc.ChannelOverrides {
			iov := ipcChannelOverride{
				ChannelID:      ov.ChannelID,
				RequireMention: ov.RequireMention,
				InboundPrefix:  ov.InboundPrefix,
				AllowedUsers:   ov.AllowedUsers,
				AllowedRoles:   ov.AllowedRoles,
			}
			overrides = append(overrides, iov)
		}
		subEnv = ipcSubscribeGuildEnv(
			r.agentName, 0, dcc.GuildID, dcc.InboundPrefix,
			dcc.AllowedUsers, dcc.AllowedRoles, dcc.RequireMention, overrides,
		)
	default:
		subEnv = ipcSubscribeChannelEnv(
			r.agentName, 0, dcc.ChannelID, dcc.InboundPrefix,
			dcc.AllowedUsers, dcc.AllowedRoles, dcc.RequireMention,
		)
	}

	if err := gwWr.Write(subEnv); err != nil {
		gwConn.Close()
		logf("discord gateway subscribe write: %v", err)
		return
	}

	ack, err := gwRd.Read()
	if err != nil {
		gwConn.Close()
		logf("discord gateway ack read: %v", err)
		return
	}
	if ack.Type == "error" {
		gwConn.Close()
		logf("discord gateway subscribe error: %s: %s", ack.Reason, ack.Detail)
		return
	}
	if ack.Type != "subscribe-ack" {
		gwConn.Close()
		logf("discord gateway unexpected ack type: %s", ack.Type)
		return
	}

	gwc := &routerGWConn{
		platform: "discord",
		account:  dcc.Account,
		conn:     gwConn,
		rd:       gwRd,
		wr:       gwWr,
	}
	r.mu.Lock()
	r.gwConns[gwKey] = gwc
	r.mu.Unlock()

	logf("subscribed discord gateway account=%s mode=%s", dcc.Account, dcc.SubscriptionMode)

	// Pump inbound messages from the gateway to the shim.
	go r.pumpGateway(ctx, gwc, logf)
}

// pumpGateway reads inbound envelopes from a gateway and forwards them to the
// shim with Source stamped.
func (r *agentRouter) pumpGateway(ctx context.Context, gwc *routerGWConn, logf func(string, ...any)) {
	for {
		env, err := gwc.rd.Read()
		if err != nil {
			if ctx.Err() == nil {
				logf("gateway read err platform=%s account=%s: %v", gwc.platform, gwc.account, err)
			}
			return
		}
		if env.Type != "inbound" {
			continue
		}
		// Stamp the source so the shim can surface it as meta.source.
		env.Source = gwc.platform

		r.mu.Lock()
		wr := r.shimWr
		r.mu.Unlock()
		if wr == nil {
			return
		}
		if err := wr.Write(env); err != nil {
			logf("shim write err: %v", err)
			return
		}
	}
}

// handleShimUnsubscribe propagates an unsubscribe to all gateway connections.
func (r *agentRouter) handleShimUnsubscribe(env ipcEnvelope, logf func(string, ...any)) {
	logf("unsubscribe from shim agent=%s", r.agentName)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, gwc := range r.gwConns {
		_ = gwc.wr.Write(ipcUnsubscribeEnv(env.RoutingKey))
	}
}

// handleShimSend parses the Target URL and routes the send to the correct
// gateway. Supported in slice 1: discord://channel/<id>, discord://dm/<userId>.
func (r *agentRouter) handleShimSend(env ipcEnvelope, logf func(string, ...any)) {
	target := env.Target
	if target == "" {
		logf("send without Target, dropping")
		return
	}

	platform, subtype, id, parseErr := parseTargetURL(target)
	if parseErr != "" {
		logf("send target parse error: %s", parseErr)
		r.sendShimError("bad-target", parseErr)
		return
	}

	switch platform {
	case "discord":
		r.routeDiscordSend(env, subtype, id, logf)
	default:
		msg := "unknown target scheme: " + platform
		logf(msg)
		r.sendShimError("bad-target", msg)
	}
}

// routeDiscordSend finds the discord gateway connection and forwards a send.
// subtype is "channel" or "dm"; id is the channelId or userId.
func (r *agentRouter) routeDiscordSend(env ipcEnvelope, subtype, id string, logf func(string, ...any)) {
	// Find the discord gateway connection (any account for now — slice 1
	// assumes one discord account per agent; slice 2 can add account routing).
	r.mu.Lock()
	var gwc *routerGWConn
	for _, gw := range r.gwConns {
		if gw.platform == "discord" {
			gwc = gw
			break
		}
	}
	r.mu.Unlock()

	if gwc == nil {
		logf("no discord gateway connection for send target=%s", env.Target)
		r.sendShimError("no-gateway", "no discord gateway connected")
		return
	}

	// For DMs the gateway needs a channel ID, not a user ID. discordgo's
	// UserChannelCreate handles open-or-reuse. We do it at the router level
	// so the gateway stays unaware of DM semantics.
	channelID := id
	if subtype == "dm" {
		var err error
		channelID, err = openDiscordDM(gwc, id, logf)
		if err != nil {
			logf("open discord DM for user=%s: %v", id, err)
			r.sendShimError("dm-open-failed", err.Error())
			return
		}
	}

	sendEnv := ipcSendEnv(channelID, env.Content)
	if err := gwc.wr.Write(sendEnv); err != nil {
		logf("discord gateway send write: %v", err)
		r.sendShimError("send-failed", err.Error())
		return
	}
	logf("routed send to discord account=%s channelId=%s len=%d", gwc.account, channelID, len(env.Content))
}

// openDiscordDM asks the discord gateway to open (or reuse) a DM channel with
// the given userID and returns the resulting channelID. We send a synthetic
// "dm-open" envelope; the gateway responds with a "dm-open-ack" carrying the
// channelID. Slice-1 simplification: no actual gateway protocol extension —
// we use the list-channels call to get visible channels and fall back to a
// direct discordgo call. For now, we expose this via a simple custom envelope
// handled in the gateway. Until the gateway handles it, we embed the
// discordgo UserChannelCreate call here via a gateway-level "dm-open" type.
//
// Actually: the discord gateway doesn't yet have a dm-open handler. For slice
// 1, DM opens are handled by the router sending a special envelope that the
// gateway responds to. We add this protocol extension to the gateway.
// For simplicity in slice 1, we send the dm-open request and wait for the ack.
func openDiscordDM(gwc *routerGWConn, userID string, logf func(string, ...any)) (string, error) {
	req := ipcEnvelope{V: ipcVersion, Type: "dm-open", UserID: userID}
	if err := gwc.wr.Write(req); err != nil {
		return "", fmt.Errorf("dm-open write: %w", err)
	}
	// The gateway may send inbound envelopes before our ack; drain until we
	// see the dm-open-ack. With a 10s timeout.
	gwc.conn.SetDeadline(time.Now().Add(10 * time.Second)) //nolint
	defer gwc.conn.SetDeadline(time.Time{})                //nolint
	for {
		resp, err := gwc.rd.Read()
		if err != nil {
			return "", fmt.Errorf("dm-open read: %w", err)
		}
		if resp.Type == "dm-open-ack" {
			return resp.ChannelID, nil
		}
		if resp.Type == "error" {
			return "", fmt.Errorf("%s: %s", resp.Reason, resp.Detail)
		}
		logf("dm-open: unexpected envelope type=%s (ignored)", resp.Type)
	}
}

func (r *agentRouter) sendShimError(reason, detail string) {
	r.mu.Lock()
	wr := r.shimWr
	r.mu.Unlock()
	if wr != nil {
		_ = wr.Write(ipcErrorEnv(reason, detail))
	}
}

// parseTargetURL parses a URL-shape target string into (platform, subtype, id).
// Returns a non-empty error string on failure.
//
// Supported forms (slice 1):
//
//	discord://channel/<channelId>
//	discord://dm/<userId>
func parseTargetURL(target string) (platform, subtype, id, errMsg string) {
	// Minimal URL parse without importing net/url to keep things simple.
	// Format: <scheme>://<subtype>/<id>
	var rest string
	for _, sep := range []string{"://"} {
		if idx := indexOf(target, sep); idx >= 0 {
			platform = target[:idx]
			rest = target[idx+len(sep):]
			break
		}
	}
	if platform == "" {
		return "", "", "", "target missing scheme (expected scheme://subtype/id)"
	}
	slashIdx := indexOf(rest, "/")
	if slashIdx < 0 {
		return "", "", "", "target missing subtype (expected scheme://subtype/id)"
	}
	subtype = rest[:slashIdx]
	id = rest[slashIdx+1:]
	if id == "" {
		return "", "", "", "target missing id (expected scheme://subtype/id)"
	}
	return platform, subtype, id, ""
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// routerSubsFile is the path of the file the router writes so the shim can
// read its subscription list on startup without a separate RPC.
func routerSubsFile(agentName string) (string, error) {
	dir, err := agentWorkdir(agentName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mesh-subs.json"), nil
}

// routerSubscription is the wire shape written to mesh-subs.json.
type routerSubscription struct {
	Platform string         `json:"platform"`
	Config   map[string]any `json:"config"`
}

func writeRouterSubsFile(agentName string, channels []ChannelSpec) {
	subs := make([]routerSubscription, 0, len(channels))
	for _, ch := range channels {
		subs = append(subs, routerSubscription{Platform: ch.Type, Config: ch.Config})
	}
	path, err := routerSubsFile(agentName)
	if err != nil {
		return
	}
	data, err := json.MarshalIndent(subs, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWriteFile(path, data, 0o600)
}

// ReadRouterSubscriptions reads the subscriptions the daemon wrote for this
// agent. Used by the mesh-shim on startup to learn its backends without a
// separate IPC call.
func ReadRouterSubscriptions(agentName string) ([]routerSubscription, error) {
	path, err := routerSubsFile(agentName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read router subs %s: %w", path, err)
	}
	var subs []routerSubscription
	if err := json.Unmarshal(data, &subs); err != nil {
		return nil, fmt.Errorf("parse router subs: %w", err)
	}
	return subs, nil
}

func routerLogf(agentName string) func(string, ...any) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".mesh", "agents", agentName, "channels")
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, fmt.Sprintf("mesh-router-%d.log", os.Getpid()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "[mesh-router] "+format+"\n", args...)
		}
	}
	return func(format string, args ...any) {
		fmt.Fprintf(f, "["+time.Now().UTC().Format(time.RFC3339Nano)+"] "+format+"\n", args...)
	}
}
