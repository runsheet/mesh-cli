package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/ipc"
	"projectmesh.io/mesh-cli/internal/paths"
	"projectmesh.io/mesh-cli/internal/peers"
)

// AgentEntry is the minimal slice of agent definition that ChannelRouter needs
// to reconcile. Callers in daemon/localsock build []AgentEntry from
// []*state.AgentDef before calling Sync, keeping internal/channels free of any
// import of internal/state.
type AgentEntry struct {
	Name        string
	Description string
	Channels    []Spec
}

// MeshRouterSockPath returns the path of the per-agent router socket.
// The router (daemon-side) listens here; the mesh-shim connects to it.
func MeshRouterSockPath(agentName string) (string, error) { return paths.RouterSockPath(agentName) }

// RouterDeps holds optional callbacks the daemon injects into the router so
// that shim-initiated task operations can reach mesh-api without creating
// an import cycle (daemon → channels is fine; channels → daemon is not).
//
// Any nil field means the corresponding capability is unavailable; the router
// returns an appropriate error envelope to the shim.
type RouterDeps struct {
	// TaskGet fetches a single task by UUID. Returns the raw JSON body
	// from mesh-api (the full response object) or an error.
	TaskGet func(id string) ([]byte, error)
	// TaskUpdate POSTs to /task/update. payload must include taskId.
	TaskUpdate func(payload map[string]any) ([]byte, error)
	// TaskHandoff POSTs to /task/handoff. payload must include taskId, fromRole, toRole.
	TaskHandoff func(payload map[string]any) ([]byte, error)
	// OnTaskActivity is called after a successful task-update or
	// task-handoff from the shim. The daemon uses it to mark the agent
	// active so the poll cadence shortens from 300s to 60s.
	OnTaskActivity func(agentName string)
	// Owner is the ChannelRouter that owns this router. Set by startRouterLocked
	// so serveShim can register/deregister the poke-writer on connect/disconnect.
	Owner *ChannelRouter
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
// reconnects when needed.
type agentRouter struct {
	agentName   string
	description string
	channels    []Spec
	broker      *peers.Broker
	deps        RouterDeps

	mu       sync.Mutex
	shimConn net.Conn           // single connected shim (nil if none)
	shimWr   *ipc.Writer
	gwConns  map[string]*routerGWConn // key: "platform:account"
	brokerCh chan ipc.Envelope         // non-nil when registered with broker
}

type routerGWConn struct {
	platform string
	account  string
	conn     net.Conn
	rd       *ipc.Reader
	wr       *ipc.Writer
}

// ChannelRouter manages one agentRouter per agent. The daemon creates a
// ChannelRouter and calls Sync on every agent-set change.
type ChannelRouter struct {
	mu          sync.Mutex
	routers     map[string]*runningRouter // key: agentName
	pokeWriters map[string]func(taskID, reason string) bool
	broker      *peers.Broker
	deps        RouterDeps
	startFn     func(ctx context.Context, agentName, description string, channels []Spec, broker *peers.Broker, deps RouterDeps)
}

type runningRouter struct {
	cancel   context.CancelFunc
	done     chan struct{}
	channels []Spec
}

// NewChannelRouter creates an empty ChannelRouter backed by the real runAgentRouter.
func NewChannelRouter(broker *peers.Broker) *ChannelRouter {
	return &ChannelRouter{
		routers:     map[string]*runningRouter{},
		pokeWriters: map[string]func(taskID, reason string) bool{},
		broker:      broker,
		startFn:     runAgentRouterWithDeps,
	}
}

// SetDeps injects optional daemon callbacks (task HTTP helpers). Must be
// called before the first Sync; safe to call once at startup.
func (cr *ChannelRouter) SetDeps(deps RouterDeps) {
	cr.mu.Lock()
	cr.deps = deps
	cr.mu.Unlock()
}

// NewChannelRouterWithStart creates a ChannelRouter with a custom start function.
// Used in tests to inject a stub without running real goroutines. The stub
// receives a zero-value RouterDeps; tests that need deps should use SetDeps.
func NewChannelRouterWithStart(broker *peers.Broker, fn func(ctx context.Context, agentName, description string, channels []Spec, broker *peers.Broker)) *ChannelRouter {
	return &ChannelRouter{
		routers:     map[string]*runningRouter{},
		pokeWriters: map[string]func(taskID, reason string) bool{},
		broker:      broker,
		startFn: func(ctx context.Context, agentName, description string, channels []Spec, broker *peers.Broker, _ RouterDeps) {
			fn(ctx, agentName, description, channels, broker)
		},
	}
}

// Sync reconciles running routers against the provided agent list.
// Routers for agents that have been removed are stopped; new agents get a
// fresh router goroutine.
func (cr *ChannelRouter) Sync(entries []AgentEntry) {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	desired := map[string]*AgentEntry{}
	for i := range entries {
		a := &entries[i]
		if len(a.Channels) > 0 {
			desired[a.Name] = a
		}
	}

	for name, a := range desired {
		rr, running := cr.routers[name]
		if !running {
			cr.startRouterLocked(name, a.Description, a.Channels)
			continue
		}
		if !reflect.DeepEqual(rr.channels, a.Channels) {
			rr.cancel()
			<-rr.done
			delete(cr.routers, name)
			cr.startRouterLocked(name, a.Description, a.Channels)
		}
	}

	for name, rr := range cr.routers {
		if _, want := desired[name]; !want {
			rr.cancel()
			<-rr.done
			delete(cr.routers, name)
		}
	}
}

// DeliverPoke injects a task poke directly into the shim's inbound
// stream as a notifications/claude/channel notification with
// meta.source == "mesh-poke". This replaces the old PTY-socket + sidecar
// delivery path. Returns false when no shim is connected for the agent
// (caller logs and moves on — same semantics as the old "no listener" path).
func (cr *ChannelRouter) DeliverPoke(agentName, taskID, reason string) bool {
	cr.mu.Lock()
	fn := cr.pokeWriters[agentName]
	cr.mu.Unlock()
	if fn == nil {
		return false
	}
	return fn(taskID, reason)
}

// registerPokeWriter is called by agentRouter.serveShim when a shim connects.
func (cr *ChannelRouter) registerPokeWriter(agentName string, fn func(taskID, reason string) bool) {
	if cr == nil {
		return
	}
	cr.mu.Lock()
	cr.pokeWriters[agentName] = fn
	cr.mu.Unlock()
}

// deregisterPokeWriter is called by agentRouter.serveShim when a shim disconnects.
func (cr *ChannelRouter) deregisterPokeWriter(agentName string) {
	if cr == nil {
		return
	}
	cr.mu.Lock()
	delete(cr.pokeWriters, agentName)
	cr.mu.Unlock()
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

func (cr *ChannelRouter) startRouterLocked(agentName, description string, channels []Spec) {
	ctx, cancel := context.WithCancel(context.Background())
	rr := &runningRouter{cancel: cancel, done: make(chan struct{}), channels: channels}
	cr.routers[agentName] = rr
	broker := cr.broker
	deps := cr.deps
	deps.Owner = cr
	startFn := cr.startFn
	go func() {
		defer close(rr.done)
		startFn(ctx, agentName, description, channels, broker, deps)
	}()
}

// runAgentRouterWithDeps is the goroutine that owns one agent's mesh.sock listener.
// deps carries optional daemon callbacks; it is zero-value when invoked from tests.
func runAgentRouterWithDeps(ctx context.Context, agentName, description string, channels []Spec, broker *peers.Broker, deps RouterDeps) {
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

	writeRouterSubsFile(agentName, channels)

	for {
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
				agentName:   agentName,
				description: description,
				channels:    channels,
				broker:      broker,
				deps:        deps,
				gwConns:     map[string]*routerGWConn{},
			}
			r.serveShim(ctx, conn, logf)
		}
	}
}

// serveShim is the per-shim-connection handler.
func (r *agentRouter) serveShim(ctx context.Context, conn net.Conn, logf func(string, ...any)) {
	defer conn.Close()

	r.mu.Lock()
	r.shimConn = conn
	r.shimWr = ipc.NewWriter(conn)
	r.mu.Unlock()

	// Register a poke-writer so ChannelRouter.DeliverPoke can reach this shim.
	if r.deps.Owner != nil {
		r.deps.Owner.registerPokeWriter(r.agentName, func(taskID, reason string) bool {
			r.mu.Lock()
			wr := r.shimWr
			r.mu.Unlock()
			if wr == nil {
				return false
			}
			text := fmt.Sprintf("mesh:task-poke id=%s reason=%s", taskID, reason)
			env := ipc.Envelope{
				V:       ipc.Version,
				Type:    "inbound",
				Source:  "mesh-poke",
				Content: text,
			}
			if err := wr.Write(env); err != nil {
				logf("poke write err agent=%s: %v", r.agentName, err)
				return false
			}
			logf("poke delivered agent=%s task=%s reason=%s", r.agentName, taskID, reason)
			return true
		})
	}

	defer func() {
		if r.deps.Owner != nil {
			r.deps.Owner.deregisterPokeWriter(r.agentName)
		}

		r.mu.Lock()
		r.shimConn = nil
		r.shimWr = nil
		for _, gw := range r.gwConns {
			gw.conn.Close()
		}
		r.gwConns = map[string]*routerGWConn{}
		hadBroker := r.brokerCh != nil
		r.brokerCh = nil
		r.mu.Unlock()

		if hadBroker && r.broker != nil {
			r.broker.Unregister(r.agentName)
		}
	}()

	rd := ipc.NewReader(conn)
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
			r.handleShimSubscribe(ctx, logf)
		case "unsubscribe":
			r.handleShimUnsubscribe(env, logf)
		case "send":
			r.handleShimSend(env, logf)
		case "peers-list":
			r.handleShimPeersList(env, logf)
		case "peers-set-summary":
			r.handleShimPeersSetSummary(env, logf)
		case "task-get":
			r.handleShimTaskGet(env, logf)
		case "task-update":
			r.handleShimTaskUpdate(env, logf)
		case "task-handoff":
			r.handleShimTaskHandoff(env, logf)
		default:
			logf("unknown shim envelope type=%s", env.Type)
		}
	}
}

// handleShimSubscribe fans the subscribe out to every gateway referenced in
// the agent's channel list.
func (r *agentRouter) handleShimSubscribe(ctx context.Context, logf func(string, ...any)) {
	logf("subscribe from shim agent=%s", r.agentName)

	for _, ch := range r.channels {
		switch ch.Type {
		case "discord":
			go r.resubscribeGateway(ctx, ch, logf)
		case "whatsapp":
			go r.resubscribeGateway(ctx, ch, logf)
		case "peers":
			r.subscribePeers(ctx, ch, logf)
		}
	}

	r.mu.Lock()
	wr := r.shimWr
	r.mu.Unlock()
	if wr != nil {
		_ = wr.Write(ipc.AckEnv("agent:" + r.agentName))
	}
}

const (
	gwResubscribeInitialDelay = 1 * time.Second
	gwResubscribeMaxDelay     = 30 * time.Second
)

// gwAccount extracts the account identifier from a channel spec for logging.
func gwAccount(ch Spec) string {
	if a, ok := ch.Config["account"].(string); ok {
		return a
	}
	return "unknown"
}

// resubscribeGateway is the connect-and-reconnect loop for a single gateway
// channel. On first call it connects immediately (no sleep). When pumpGateway
// exits due to a transport error, it sleeps with exponential backoff and
// retries. Protocol errors (the gateway returns an "error" envelope during the
// subscribe handshake) are logged once and the loop exits without retrying.
// The loop exits when ctx is cancelled.
func (r *agentRouter) resubscribeGateway(ctx context.Context, ch Spec, logf func(string, ...any)) {
	delay := gwResubscribeInitialDelay
	reconnect := false // false on the first pass — dial immediately
	for {
		if ctx.Err() != nil {
			return
		}

		if reconnect {
			logf("gateway resubscribing platform=%s account=%s delay=%ds",
				ch.Type, gwAccount(ch), int(delay.Seconds()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay *= 2
			if delay > gwResubscribeMaxDelay {
				delay = gwResubscribeMaxDelay
			}
		}

		var connected, retryable bool
		switch ch.Type {
		case "discord":
			connected, retryable = r.subscribeDiscordGateway(ctx, ch, logf)
		case "whatsapp":
			connected, retryable = r.subscribeWhatsappGateway(ctx, ch, logf)
		default:
			return
		}

		if !retryable {
			// Protocol error — gateway explicitly rejected subscription. Don't retry.
			return
		}

		if ctx.Err() != nil {
			return
		}

		if connected {
			// The connection was established and pumpGateway ran until it dropped.
			// Log and reset backoff — the next attempt starts fresh.
			logf("gateway connection dropped platform=%s account=%s reconnecting", ch.Type, gwAccount(ch))
			delay = gwResubscribeInitialDelay
		}
		reconnect = true
	}
}

// subscribeDiscordGateway dials the discord gateway, performs the subscribe
// handshake, registers the conn, and blocks in pumpGateway until the conn
// drops or ctx is cancelled.
//
// Returns (connected=true, retryable=true)  — handshake succeeded, pumpGateway ran, then dropped.
// Returns (connected=false, retryable=true) — transport error before handshake; caller should retry.
// Returns (connected=false, retryable=false) — protocol error; don't retry.
func (r *agentRouter) subscribeDiscordGateway(ctx context.Context, ch Spec, logf func(string, ...any)) (connected, retryable bool) {
	dcc := ParseDiscordChannelConfig(ch.Config)
	if dcc.Account == "" {
		logf("discord channel missing account")
		return false, false
	}
	gwKey := gatewayKey("discord", dcc.Account)

	r.mu.Lock()
	_, already := r.gwConns[gwKey]
	r.mu.Unlock()
	if already {
		// Already connected — pumpGateway is still running.
		return true, true
	}

	sockPath, err := paths.DiscordGatewaySockPath(dcc.Account)
	if err != nil {
		logf("discord gateway sock path: %v", err)
		return false, false
	}

	var (
		gwConn net.Conn
		gwRd   *ipc.Reader
		gwWr   *ipc.Writer
	)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if ctx.Err() != nil {
			return false, true
		}
		gwConn, gwRd, gwWr, err = ipc.DialGateway(sockPath)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			logf("discord gateway dial deadline: %v", err)
			return false, true
		}
		logf("discord gateway dial retry: %v", err)
		time.Sleep(200 * time.Millisecond)
	}

	var subEnv ipc.Envelope
	switch dcc.SubscriptionMode {
	case "guild":
		overrides := make([]ipc.ChannelOverride, 0, len(dcc.ChannelOverrides))
		for _, ov := range dcc.ChannelOverrides {
			overrides = append(overrides, ipc.ChannelOverride{
				ChannelID:      ov.ChannelID,
				RequireMention: ov.RequireMention,
				InboundPrefix:  ov.InboundPrefix,
				AllowedUsers:   ov.AllowedUsers,
				AllowedRoles:   ov.AllowedRoles,
			})
		}
		subEnv = ipc.SubscribeGuildEnv(
			r.agentName, 0, dcc.GuildID, dcc.InboundPrefix,
			dcc.AllowedUsers, dcc.AllowedRoles, dcc.RequireMention, overrides,
		)
	default:
		subEnv = ipc.SubscribeChannelEnv(
			r.agentName, 0, dcc.ChannelID, dcc.InboundPrefix,
			dcc.AllowedUsers, dcc.AllowedRoles, dcc.RequireMention,
		)
	}

	if err := gwWr.Write(subEnv); err != nil {
		gwConn.Close()
		logf("discord gateway subscribe write: %v", err)
		return false, true
	}

	ack, err := gwRd.Read()
	if err != nil {
		gwConn.Close()
		logf("discord gateway ack read: %v", err)
		return false, true
	}
	if ack.Type == "error" {
		gwConn.Close()
		logf("discord gateway subscribe error: %s: %s", ack.Reason, ack.Detail)
		return false, false
	}
	if ack.Type != "subscribe-ack" {
		gwConn.Close()
		logf("discord gateway unexpected ack type: %s", ack.Type)
		return false, false
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
	r.pumpGateway(ctx, gwc, gwKey, logf)
	return true, true
}

// pumpGateway reads inbound envelopes from a gateway and forwards them to
// the shim with Source stamped. gwKey is the map key used to remove the stale
// entry on exit so that resubscribeGateway can dial fresh.
func (r *agentRouter) pumpGateway(ctx context.Context, gwc *routerGWConn, gwKey string, logf func(string, ...any)) {
	// Close the conn when ctx is cancelled so that gwc.rd.Read() unblocks.
	stopClose := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			gwc.conn.Close()
		case <-stopClose:
		}
	}()

	defer func() {
		close(stopClose)
		// Remove the stale conn so the next subscribe attempt can re-dial.
		r.mu.Lock()
		delete(r.gwConns, gwKey)
		r.mu.Unlock()
		gwc.conn.Close()
	}()

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

// FilterSelf removes any peer whose PeerID equals self from the slice.
func FilterSelf(ps []peers.Registration, self string) []peers.Registration {
	out := ps[:0:0]
	for _, p := range ps {
		if p.PeerID != self {
			out = append(out, p)
		}
	}
	return out
}

// handleShimPeersList handles a peers-list request from the shim.
func (r *agentRouter) handleShimPeersList(env ipc.Envelope, logf func(string, ...any)) {
	team := env.RoutingKey

	var entries []ipc.PeerEntryWire
	if r.broker != nil {
		ps := FilterSelf(r.broker.ListPeers(team), r.agentName)
		entries = make([]ipc.PeerEntryWire, 0, len(ps))
		for _, p := range ps {
			entries = append(entries, ipc.PeerEntryWire{
				PeerID:     p.PeerID,
				NodeID:     p.NodeID,
				Teams:      p.Teams,
				LastSeenAt: p.LastSeenAt.UTC().Format(time.RFC3339),
				Summary:    p.Summary,
			})
		}
	}

	data, _ := json.Marshal(entries)
	r.mu.Lock()
	wr := r.shimWr
	r.mu.Unlock()
	if wr != nil {
		_ = wr.Write(ipc.PeersListRespEnv(string(data)))
	}
	logf("peers-list agent=%s team=%q peers=%d", r.agentName, team, len(entries))
}

// handleShimPeersSetSummary handles a peers-set-summary request from the shim.
func (r *agentRouter) handleShimPeersSetSummary(env ipc.Envelope, logf func(string, ...any)) {
	summary := env.Content
	r.mu.Lock()
	wr := r.shimWr
	r.mu.Unlock()
	if wr == nil {
		return
	}
	if summary == "" {
		_ = wr.Write(ipc.PeersSetSummaryRespEnv("summary is required"))
		return
	}
	if r.broker == nil {
		_ = wr.Write(ipc.PeersSetSummaryRespEnv("peers broker not available"))
		return
	}
	ok := r.broker.SetSummary(r.agentName, summary)
	if !ok {
		_ = wr.Write(ipc.PeersSetSummaryRespEnv("agent not registered with peers broker (add a peers channel to your template)"))
		return
	}
	logf("peers-set-summary agent=%s len=%d", r.agentName, len(summary))
	_ = wr.Write(ipc.PeersSetSummaryRespEnv(""))
}

// handleShimUnsubscribe propagates an unsubscribe to all gateway connections.
func (r *agentRouter) handleShimUnsubscribe(env ipc.Envelope, logf func(string, ...any)) {
	logf("unsubscribe from shim agent=%s", r.agentName)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, gwc := range r.gwConns {
		_ = gwc.wr.Write(ipc.UnsubscribeEnv(env.RoutingKey))
	}
}

// handleShimSend parses the Target URL and routes the send to the correct gateway.
func (r *agentRouter) handleShimSend(env ipc.Envelope, logf func(string, ...any)) {
	target := env.Target
	if target == "" {
		logf("send without Target, dropping")
		return
	}

	platform, subtype, id, parseErr := ParseTargetURL(target)
	if parseErr != "" {
		logf("send target parse error: %s", parseErr)
		r.sendShimError("bad-target", parseErr)
		return
	}

	switch platform {
	case "discord":
		r.routeDiscordSend(env, subtype, id, logf)
	case "whatsapp":
		r.routeWhatsappSend(env, subtype, id, logf)
	case "peers":
		r.routePeersSend(env, subtype, id, logf)
	default:
		msg := "unknown target scheme: " + platform
		logf(msg)
		r.sendShimError("bad-target", msg)
	}
}

// routeDiscordSend finds the discord gateway connection and forwards a send.
func (r *agentRouter) routeDiscordSend(env ipc.Envelope, subtype, id string, logf func(string, ...any)) {
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

	sendEnv := ipc.SendEnvWithAttachments(env.Target, channelID, env.Content, env.Attachments)
	if err := gwc.wr.Write(sendEnv); err != nil {
		logf("discord gateway send write: %v", err)
		r.sendShimError("send-failed", err.Error())
		return
	}
	logf("routed send to discord account=%s channelId=%s len=%d attachments=%d", gwc.account, channelID, len(env.Content), len(env.Attachments))
}

// subscribeWhatsappGateway connects this router to the whatsapp gateway.
// Returns (connected, retryable) matching the same contract as subscribeDiscordGateway.
func (r *agentRouter) subscribeWhatsappGateway(ctx context.Context, ch Spec, logf func(string, ...any)) (connected, retryable bool) {
	wcc := ParseWhatsappChannelConfig(ch.Config)
	if wcc.Account == "" {
		logf("whatsapp channel missing account")
		return false, false
	}
	gwKey := gatewayKey("whatsapp", wcc.Account)

	r.mu.Lock()
	_, already := r.gwConns[gwKey]
	r.mu.Unlock()
	if already {
		return true, true
	}

	sockPath, err := paths.WhatsappGatewaySockPath(wcc.Account)
	if err != nil {
		logf("whatsapp gateway sock path: %v", err)
		return false, false
	}

	var (
		gwConn net.Conn
		gwRd   *ipc.Reader
		gwWr   *ipc.Writer
	)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if ctx.Err() != nil {
			return false, true
		}
		gwConn, gwRd, gwWr, err = ipc.DialGateway(sockPath)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			logf("whatsapp gateway dial deadline: %v", err)
			return false, true
		}
		logf("whatsapp gateway dial retry: %v", err)
		time.Sleep(200 * time.Millisecond)
	}

	subEnv := ipc.SubscribeChannelEnv(r.agentName, 0, wcc.ChatJID, "", nil, nil, false)
	if err := gwWr.Write(subEnv); err != nil {
		gwConn.Close()
		logf("whatsapp gateway subscribe write: %v", err)
		return false, true
	}
	ack, err := gwRd.Read()
	if err != nil {
		gwConn.Close()
		logf("whatsapp gateway ack read: %v", err)
		return false, true
	}
	if ack.Type == "error" {
		gwConn.Close()
		logf("whatsapp gateway subscribe error: %s: %s", ack.Reason, ack.Detail)
		return false, false
	}
	if ack.Type != "subscribe-ack" {
		gwConn.Close()
		logf("whatsapp gateway unexpected ack type: %s", ack.Type)
		return false, false
	}

	gwc := &routerGWConn{
		platform: "whatsapp",
		account:  wcc.Account,
		conn:     gwConn,
		rd:       gwRd,
		wr:       gwWr,
	}
	r.mu.Lock()
	r.gwConns[gwKey] = gwc
	r.mu.Unlock()

	logf("subscribed whatsapp gateway account=%s jid=%s", wcc.Account, wcc.ChatJID)
	r.pumpGateway(ctx, gwc, gwKey, logf)
	return true, true
}

// routeWhatsappSend routes an outbound send to the whatsapp gateway.
func (r *agentRouter) routeWhatsappSend(env ipc.Envelope, subtype, id string, logf func(string, ...any)) {
	if subtype != "chat" {
		msg := "whatsapp://: unsupported namespace " + subtype + " (only chat/<jid> is supported)"
		logf(msg)
		r.sendShimError("bad-target", msg)
		return
	}
	if !IsValidWhatsappJID(id) {
		msg := "whatsapp://chat/: invalid JID " + id
		logf(msg)
		r.sendShimError("bad-target", msg)
		return
	}

	r.mu.Lock()
	var gwc *routerGWConn
	for _, gw := range r.gwConns {
		if gw.platform == "whatsapp" {
			gwc = gw
			break
		}
	}
	r.mu.Unlock()

	if gwc == nil {
		logf("no whatsapp gateway connection for send target=%s", env.Target)
		r.sendShimError("no-gateway", "no whatsapp gateway connected")
		return
	}

	sendEnv := ipc.SendEnvWithAttachments(env.Target, id, env.Content, env.Attachments)
	if err := gwc.wr.Write(sendEnv); err != nil {
		logf("whatsapp gateway send write: %v", err)
		r.sendShimError("send-failed", err.Error())
		return
	}
	logf("routed send to whatsapp account=%s jid=%s len=%d attachments=%d", gwc.account, id, len(env.Content), len(env.Attachments))
}

// subscribePeers registers this agent with the broker.
func (r *agentRouter) subscribePeers(ctx context.Context, ch Spec, logf func(string, ...any)) {
	if r.broker == nil {
		logf("peers channel configured but no broker provided")
		return
	}
	pcc := ParsePeersChannelConfig(ch.Config)

	deliverCh := make(chan ipc.Envelope, 64)
	reg := peers.Registration{
		PeerID:  r.agentName,
		NodeID:  "local",
		Teams:   pcc.Teams,
		Summary: r.description,
	}
	r.broker.Register(reg, deliverCh)

	r.mu.Lock()
	r.brokerCh = deliverCh
	r.mu.Unlock()

	logf("registered with peers broker agent=%s teams=%v", r.agentName, pcc.Teams)
	go r.pumpBroker(ctx, deliverCh, logf)
}

// pumpBroker reads inbound envelopes from the broker delivery channel.
func (r *agentRouter) pumpBroker(ctx context.Context, deliverCh chan ipc.Envelope, logf func(string, ...any)) {
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-deliverCh:
			if !ok {
				return
			}
			r.mu.Lock()
			wr := r.shimWr
			r.mu.Unlock()
			if wr == nil {
				return
			}
			if err := wr.Write(env); err != nil {
				logf("peers broker shim write err: %v", err)
				return
			}
		}
	}
}

// routePeersSend routes an outbound send from the shim to the peers broker.
func (r *agentRouter) routePeersSend(env ipc.Envelope, subtype, id string, logf func(string, ...any)) {
	if r.broker == nil {
		r.sendShimError("no-broker", "peers broker not available")
		return
	}
	switch subtype {
	case "agent":
		if !r.broker.SendDirect(id, r.agentName, env.Content) {
			msg := "peers agent not registered: " + id
			logf(msg)
			r.sendShimError("peer-not-found", msg)
		} else {
			logf("peers send direct from=%s to=%s len=%d", r.agentName, id, len(env.Content))
		}
	case "team":
		n := r.broker.SendTeam(id, r.agentName, env.Content)
		logf("peers send team=%s from=%s reached=%d len=%d", id, r.agentName, n, len(env.Content))
	case "broadcast":
		n := r.broker.SendBroadcast(r.agentName, env.Content)
		logf("peers broadcast from=%s reached=%d len=%d", r.agentName, n, len(env.Content))
	default:
		msg := "unknown peers subtype: " + subtype + " (expected agent, team, or broadcast)"
		logf(msg)
		r.sendShimError("bad-target", msg)
	}
}

// openDiscordDM asks the discord gateway to open (or reuse) a DM channel.
func openDiscordDM(gwc *routerGWConn, userID string, logf func(string, ...any)) (string, error) {
	req := ipc.Envelope{V: ipc.Version, Type: "dm-open", UserID: userID}
	if err := gwc.wr.Write(req); err != nil {
		return "", fmt.Errorf("dm-open write: %w", err)
	}
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
		_ = wr.Write(ipc.ErrorEnv(reason, detail))
	}
}

// ParseTargetURL parses a URL-shape target string into (platform, subtype, id).
// Returns a non-empty error string on failure.
func ParseTargetURL(target string) (platform, subtype, id, errMsg string) {
	var found bool
	platform, rest, found := strings.Cut(target, "://")
	if !found {
		return "", "", "", "target missing scheme (expected scheme://subtype/id)"
	}

	if platform == "peers" && rest == "broadcast" {
		return platform, "broadcast", "", ""
	}

	subtype, id, found = strings.Cut(rest, "/")
	if !found {
		return "", "", "", "target missing subtype (expected scheme://subtype/id)"
	}
	if id == "" {
		return "", "", "", "target missing id (expected scheme://subtype/id)"
	}

	if platform == "peers" && subtype == "agent" && strings.Contains(id, "/") {
		return "", "", "", "cross-node peers not supported in scope=machine — use slice 4 (peers://agent/<node>/<name> form rejected)"
	}

	return platform, subtype, id, ""
}

// RouterSubsFile is the path of the file the router writes so the shim can
// read its subscription list on startup.
func RouterSubsFile(agentName string) (string, error) {
	return paths.RouterSubsFilePath(agentName)
}

// RouterSubscription is the wire shape written to mesh-subs.json.
type RouterSubscription struct {
	Platform string         `json:"platform"`
	Config   map[string]any `json:"config"`
}

func writeRouterSubsFile(agentName string, channels []Spec) {
	subs := make([]RouterSubscription, 0, len(channels))
	for _, ch := range channels {
		subs = append(subs, RouterSubscription{Platform: ch.Type, Config: ch.Config})
	}
	path, err := RouterSubsFile(agentName)
	if err != nil {
		return
	}
	data, err := json.MarshalIndent(subs, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWriteFile(path, data, 0o600)
}

// ReadRouterSubscriptions reads the subscriptions the daemon wrote for this agent.
func ReadRouterSubscriptions(agentName string) ([]RouterSubscription, error) {
	path, err := RouterSubsFile(agentName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read router subs %s: %w", path, err)
	}
	var subs []RouterSubscription
	if err := json.Unmarshal(data, &subs); err != nil {
		return nil, fmt.Errorf("parse router subs: %w", err)
	}
	return subs, nil
}

func routerLogf(agentName string) func(string, ...any) {
	dir, _ := paths.AgentChannelsDir(agentName)
	_ = os.MkdirAll(dir, 0o700)
	path := fmt.Sprintf("%s/mesh-router-%d.log", dir, os.Getpid())
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

func gatewayKey(platform, account string) string {
	return platform + ":" + account
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// handleShimTaskGet handles a task-get request from the shim.
// RoutingKey carries the task UUID. Responds with task-get-resp.
func (r *agentRouter) handleShimTaskGet(env ipc.Envelope, logf func(string, ...any)) {
	r.mu.Lock()
	wr := r.shimWr
	r.mu.Unlock()
	if wr == nil {
		return
	}
	if r.deps.TaskGet == nil {
		_ = wr.Write(ipc.TaskGetRespEnv("", "task-get not available (daemon not connected to mesh-api)"))
		return
	}
	id := env.RoutingKey
	if id == "" {
		_ = wr.Write(ipc.TaskGetRespEnv("", "task-get: id is required"))
		return
	}
	body, err := r.deps.TaskGet(id)
	if err != nil {
		logf("task-get id=%s err=%v", id, err)
		_ = wr.Write(ipc.TaskGetRespEnv("", err.Error()))
		return
	}
	logf("task-get id=%s ok", id)
	_ = wr.Write(ipc.TaskGetRespEnv(string(body), ""))
}

// handleShimTaskUpdate handles a task-update request from the shim.
// Content carries the JSON-encoded update payload. Responds with task-update-resp.
func (r *agentRouter) handleShimTaskUpdate(env ipc.Envelope, logf func(string, ...any)) {
	r.mu.Lock()
	wr := r.shimWr
	r.mu.Unlock()
	if wr == nil {
		return
	}
	if r.deps.TaskUpdate == nil {
		_ = wr.Write(ipc.TaskUpdateRespEnv("task-update not available (daemon not connected to mesh-api)"))
		return
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(env.Content), &payload); err != nil {
		_ = wr.Write(ipc.TaskUpdateRespEnv("task-update: invalid JSON payload: " + err.Error()))
		return
	}
	if _, err := r.deps.TaskUpdate(payload); err != nil {
		logf("task-update err=%v", err)
		_ = wr.Write(ipc.TaskUpdateRespEnv(err.Error()))
		return
	}
	logf("task-update ok id=%v", payload["taskId"])
	if r.deps.OnTaskActivity != nil {
		r.deps.OnTaskActivity(r.agentName)
	}
	_ = wr.Write(ipc.TaskUpdateRespEnv(""))
}

// handleShimTaskHandoff handles a task-handoff request from the shim.
// Content carries the JSON-encoded handoff payload. Responds with task-handoff-resp.
func (r *agentRouter) handleShimTaskHandoff(env ipc.Envelope, logf func(string, ...any)) {
	r.mu.Lock()
	wr := r.shimWr
	r.mu.Unlock()
	if wr == nil {
		return
	}
	if r.deps.TaskHandoff == nil {
		_ = wr.Write(ipc.TaskHandoffRespEnv("task-handoff not available (daemon not connected to mesh-api)"))
		return
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(env.Content), &payload); err != nil {
		_ = wr.Write(ipc.TaskHandoffRespEnv("task-handoff: invalid JSON payload: " + err.Error()))
		return
	}
	if _, err := r.deps.TaskHandoff(payload); err != nil {
		logf("task-handoff err=%v", err)
		_ = wr.Write(ipc.TaskHandoffRespEnv(err.Error()))
		return
	}
	logf("task-handoff ok id=%v", payload["taskId"])
	if r.deps.OnTaskActivity != nil {
		r.deps.OnTaskActivity(r.agentName)
	}
	_ = wr.Write(ipc.TaskHandoffRespEnv(""))
}
