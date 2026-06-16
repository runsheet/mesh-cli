// Package channels hosts the node-side per-agent router: a Unix-socket server
// the mesh-shim (the claude MCP "channel" server) connects to. Inbound channel
// messages the daemon receives over gRPC (ApiToNode.ChannelInbound) are written
// to the connected shim as `inbound` envelopes, which the shim surfaces to the
// live claude session as notifications/claude/channel.
//
// This is the slim post-rewrite replacement for the pre-Phase-0 channelrouter
// (which ALSO supervised in-CLI Discord/WhatsApp gateway subprocesses, ~1200
// lines). Gateways now live in mesh-api, so this router no longer dials or fans
// out to gateways — it only bridges the daemon ⇄ the per-agent shim socket.
//
// Delivery survives resume latency: DeliverInbound enqueues onto a per-agent
// buffer that outlives shim connect/disconnect, and a pump drains it to the shim
// once connected. The pump starts only AFTER the subscribe handshake, because
// the shim blocks reading subscribe-ack as its first read — an inbound written
// ahead of the ack would be mis-parsed.
//
// Outbound (mesh_send / mesh_reply / peers / task envelopes the shim writes
// back) is logged-and-ignored here for now; outbound routing + the
// NodeToApi.channel_outbound variant land in a follow-up increment.
package channels

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/ipc"
	"projectmesh.io/mesh-cli/internal/paths"
)

// inboundQueueDepth bounds the per-agent inbound buffer. Enough to absorb the
// resume window (claude boot + shim dial) without unbounded growth; a full queue
// drops oldest-style by refusing the newest (DeliverInbound returns false).
const inboundQueueDepth = 64

// Router serves one Unix socket per agent (paths.RouterSockPath) and buffers
// inbound messages for the connected shim. The map key is the agent identity
// RouterSockPath expects — the agentID, the same string the shim is spawned with
// via `mesh ... mesh-shim --agent <agentID>` (see sessions.StartWithOptions →
// claudeconf.EnsureClaudeFolderTrusted).
type Router struct {
	mu     sync.Mutex
	agents map[string]*agentConn
}

type agentConn struct {
	cancel    context.CancelFunc
	queue     chan ipc.Envelope // buffered inbound; outlives shim connect/disconnect
	connected bool              // true once the shim has subscribed
}

// New constructs an empty Router.
func New() *Router {
	return &Router{agents: map[string]*agentConn{}}
}

func routerLogf(agent, format string, args ...any) {
	fmt.Printf("mesh node router agent=%s "+format+"\n", append([]any{agent}, args...)...)
}

// Start launches the per-agent socket server if not already running. Idempotent.
// Call it when an agent's session starts, so the socket is listening before
// claude spawns the shim (which dials with a 30s retry).
func (r *Router) Start(ctx context.Context, agent string) {
	r.mu.Lock()
	if _, ok := r.agents[agent]; ok {
		r.mu.Unlock()
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	ac := &agentConn{cancel: cancel, queue: make(chan ipc.Envelope, inboundQueueDepth)}
	r.agents[agent] = ac
	r.mu.Unlock()
	go r.run(cctx, agent, ac)
}

// Stop tears down the per-agent socket server (e.g. when the session stops). Any
// still-buffered inbound is discarded with the agentConn.
func (r *Router) Stop(agent string) {
	r.mu.Lock()
	ac := r.agents[agent]
	delete(r.agents, agent)
	r.mu.Unlock()
	if ac != nil {
		ac.cancel()
	}
}

// DeliverInbound enqueues an `inbound` envelope for the agent's shim. It succeeds
// even when no shim is connected yet (the queue buffers across the resume
// window); the pump flushes it once the shim subscribes. Returns false only when
// the agent has no router (call Start first) or the queue is full.
func (r *Router) DeliverInbound(agent string, env ipc.Envelope) bool {
	r.mu.Lock()
	ac := r.agents[agent]
	r.mu.Unlock()
	if ac == nil {
		return false
	}
	select {
	case ac.queue <- env:
		return true
	default:
		routerLogf(agent, "inbound queue full — dropping message")
		return false
	}
}

// IsConnected reports whether a shim has subscribed for the agent.
func (r *Router) IsConnected(agent string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ac := r.agents[agent]
	return ac != nil && ac.connected
}

// run is the per-agent accept loop: listen on the agent's router socket and serve
// one shim connection at a time (a reconnecting shim re-enters Accept).
func (r *Router) run(ctx context.Context, agent string, ac *agentConn) {
	sockPath, err := paths.RouterSockPath(agent)
	if err != nil {
		routerLogf(agent, "router sock path: %v", err)
		return
	}
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		routerLogf(agent, "router listen %s: %v", sockPath, err)
		return
	}
	defer func() {
		ln.Close()
		_ = os.Remove(sockPath)
	}()
	_ = os.Chmod(sockPath, 0o600)
	routerLogf(agent, "router listening sock=%s", sockPath)

	for {
		connCh := make(chan net.Conn, 1)
		errCh := make(chan error, 1)
		go func() {
			c, aerr := ln.Accept()
			if aerr != nil {
				errCh <- aerr
				return
			}
			connCh <- c
		}()
		select {
		case <-ctx.Done():
			ln.Close()
			return
		case aerr := <-errCh:
			if ctx.Err() != nil {
				return
			}
			routerLogf(agent, "accept error: %v", aerr)
			time.Sleep(200 * time.Millisecond)
		case conn := <-connCh:
			r.serveShim(ctx, agent, ac, conn)
		}
	}
}

// serveShim handles one shim connection: ack `subscribe` (which the shim blocks
// on), then start the inbound pump (queue→shim) and read until EOF. The pump is
// started only after the first subscribe so subscribe-ack always precedes any
// buffered inbound. All writes to the conn go through writeEnv (the pump and the
// reader loop share the conn, so writes must be serialized).
func (r *Router) serveShim(ctx context.Context, agent string, ac *agentConn, conn net.Conn) {
	defer conn.Close()
	wr := ipc.NewWriter(conn)

	var writeMu sync.Mutex
	writeEnv := func(env ipc.Envelope) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return wr.Write(env)
	}

	pumpCtx, pumpCancel := context.WithCancel(ctx)
	defer pumpCancel()
	pumpStarted := false

	defer func() {
		r.mu.Lock()
		ac.connected = false
		r.mu.Unlock()
		routerLogf(agent, "shim disconnected")
	}()

	routerLogf(agent, "shim connected")
	rd := ipc.NewReader(conn)
	for {
		if ctx.Err() != nil {
			return
		}
		env, err := rd.Read()
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				routerLogf(agent, "shim read err: %v", err)
			}
			return
		}
		switch env.Type {
		case "subscribe":
			if err := writeEnv(ipc.AckEnv(env.RoutingKey)); err != nil {
				routerLogf(agent, "subscribe-ack write err: %v", err)
				return
			}
			routerLogf(agent, "shim subscribed routingKey=%s", env.RoutingKey)
			if !pumpStarted {
				pumpStarted = true
				r.mu.Lock()
				ac.connected = true
				r.mu.Unlock()
				go func() {
					for {
						select {
						case <-pumpCtx.Done():
							return
						case e := <-ac.queue:
							if err := writeEnv(e); err != nil {
								routerLogf(agent, "inbound pump write err: %v", err)
								pumpCancel()
								return
							}
						}
					}
				}()
			}
		case "unsubscribe":
			routerLogf(agent, "shim unsubscribed routingKey=%s", env.RoutingKey)
		default:
			// send / mesh_reply / peers-* / task-* — outbound routing (and the
			// NodeToApi.channel_outbound variant) land in a follow-up increment.
			routerLogf(agent, "outbound envelope type=%s (not yet routed)", env.Type)
		}
	}
}
