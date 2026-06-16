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

// Router serves one Unix socket per agent (paths.RouterSockPath) and tracks the
// connected shim's writer so the daemon can DeliverInbound to it. The map key is
// the agent identity RouterSockPath expects (the agent NAME, the same string the
// shim is spawned with via `mesh ... mesh-shim --agent <name>`).
type Router struct {
	mu     sync.Mutex
	agents map[string]*agentConn
}

type agentConn struct {
	cancel context.CancelFunc
	wr     *ipc.Writer // non-nil only while a shim is connected
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
	r.agents[agent] = &agentConn{cancel: cancel}
	r.mu.Unlock()
	go r.run(cctx, agent)
}

// Stop tears down the per-agent socket server (e.g. when the session stops).
func (r *Router) Stop(agent string) {
	r.mu.Lock()
	ac := r.agents[agent]
	delete(r.agents, agent)
	r.mu.Unlock()
	if ac != nil {
		ac.cancel()
	}
}

// DeliverInbound writes an `inbound` envelope to the agent's connected shim.
// Returns false when no shim is connected (the conversation's claude session is
// not up) — the caller decides whether to resume the session and retry.
func (r *Router) DeliverInbound(agent string, env ipc.Envelope) bool {
	r.mu.Lock()
	var wr *ipc.Writer
	if ac := r.agents[agent]; ac != nil {
		wr = ac.wr
	}
	r.mu.Unlock()
	if wr == nil {
		return false
	}
	if err := wr.Write(env); err != nil {
		routerLogf(agent, "inbound write err: %v", err)
		return false
	}
	return true
}

// IsConnected reports whether a shim is currently attached for the agent.
func (r *Router) IsConnected(agent string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ac := r.agents[agent]
	return ac != nil && ac.wr != nil
}

// run is the per-agent accept loop: listen on the agent's router socket and serve
// one shim connection at a time (a reconnecting shim re-enters Accept).
func (r *Router) run(ctx context.Context, agent string) {
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
			r.serveShim(ctx, agent, conn)
		}
	}
}

// serveShim handles one shim connection: register its writer, ack `subscribe`
// (which the shim blocks on), and read until EOF. Outbound envelope types are
// logged and ignored for now (outbound routing is a follow-up increment).
func (r *Router) serveShim(ctx context.Context, agent string, conn net.Conn) {
	defer conn.Close()
	wr := ipc.NewWriter(conn)

	r.mu.Lock()
	if ac := r.agents[agent]; ac != nil {
		ac.wr = wr
	}
	r.mu.Unlock()
	routerLogf(agent, "shim connected")

	defer func() {
		r.mu.Lock()
		if ac := r.agents[agent]; ac != nil {
			ac.wr = nil
		}
		r.mu.Unlock()
		routerLogf(agent, "shim disconnected")
	}()

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
			if err := wr.Write(ipc.AckEnv(env.RoutingKey)); err != nil {
				routerLogf(agent, "subscribe-ack write err: %v", err)
				return
			}
			routerLogf(agent, "shim subscribed routingKey=%s", env.RoutingKey)
		case "unsubscribe":
			routerLogf(agent, "shim unsubscribed routingKey=%s", env.RoutingKey)
		default:
			// send / mesh_reply / peers-* / task-* — outbound routing (and the
			// NodeToApi.channel_outbound variant) land in a follow-up increment.
			routerLogf(agent, "outbound envelope type=%s (not yet routed)", env.Type)
		}
	}
}
