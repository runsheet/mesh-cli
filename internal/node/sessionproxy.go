package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
	"projectmesh.io/mesh-cli/internal/sessionunit"
	"projectmesh.io/mesh-cli/internal/tools"
)

// ProxySocketName is a session proxy's socket, alone in its directory:
// a short name, since a unix socket's path is bounded (108 bytes).
const ProxySocketName = "s"

// SessionProxies starts each session's sessionproxy (mesh-cli goal
// a-session-reaches-every-mcp-server-through-one-proxy): the built-in
// runsheet and every tool the session is served are reached through one
// unix socket in a directory of the session's own under its org's MCP
// root, the only thing of the proxy a pod mounts. The proxy, on the
// host, holds what each upstream needs — the session's runsheet bearer
// in memory (Bearer), a remote tool's headers, a local tool's secret
// env — so no token file, no MESH_TOOL_* and no secret is in the pod's
// env, on its disk or in its MCP config. A nil SessionProxies proxies
// nothing: the session's servers are rendered as before (UseTools).
type SessionProxies struct {
	// Bearer is the session's runsheet bearer source: TokenFiles.Source.
	Bearer func(kind harness.Kind, meta harness.SessionMeta) (TokenSource, error)
	// Root is the MCP root the socket directories go under when the
	// session's org has none of its own (one org served: its MCP root).
	Root string
	// Logf hears the proxy's own lines, every secret masked.
	Logf func(format string, args ...any)
}

// SessionProxy is one session's running proxy: its directory and
// socket, and the first error it answered per server.
type SessionProxy struct {
	Dir    string
	Socket string
	p      *sessionproxy.Proxy
}

// FirstError is the first error the proxy answered for server name, ""
// when none (or no proxy).
func (s *SessionProxy) FirstError(name string) string {
	if s == nil || s.p == nil {
		return ""
	}
	return s.p.FirstError(name)
}

// Close stops the proxy — its socket gone, every local server it ran
// killed — and removes its directory.
func (s *SessionProxy) Close() {
	if s == nil {
		return
	}
	s.Stop()
	if s.Dir != "" {
		os.RemoveAll(s.Dir)
	}
}

// Stop stops the proxy and leaves its directory: a session detached for
// the next node keeps dialling the socket's path — a pod's mount is the
// directory itself — and the node that adopts it listens there again
// (SessionProxies.Adopt).
func (s *SessionProxy) Stop() {
	if s == nil || s.p == nil {
		return
	}
	_ = s.p.Close()
}

// End is the proxy's end with the session's ctx: Close, unless ctx is
// done — the node shutting down with the session detached — when it is
// only Stopped, its directory kept for the adopter (as unlessDetached).
func (s *SessionProxy) End(ctx context.Context) {
	if ctx.Err() != nil {
		s.Stop()
		return
	}
	s.Close()
}

// open starts a proxy for job's session under root (px.Root when ""),
// fronting upstreams, the built-in's bearer from Bearer.
func (px *SessionProxies) open(job *harness.Job, root, builtinURL string, specs map[string]tools.UpstreamSpec) (*SessionProxy, error) {
	if root == "" {
		root = px.Root
	}
	if root == "" {
		return nil, errors.New("sessionproxy: no MCP root")
	}
	if px.Bearer == nil {
		return nil, errors.New("sessionproxy: no bearer source")
	}
	bearer, err := px.Bearer(job.Kind, job.Meta)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(root, "px")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("sessionproxy: %w", err)
	}
	dir, err := os.MkdirTemp(base, "")
	if err != nil {
		return nil, fmt.Errorf("sessionproxy: %w", err)
	}
	p := sessionproxy.New(upstreamsOf(specs, builtinURL, bearer))
	p.Logf = px.Logf
	sock := filepath.Join(dir, ProxySocketName)
	if err := p.Listen(sock); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("sessionproxy: listen %s: %w", sock, err)
	}
	return &SessionProxy{Dir: dir, Socket: sock, p: p}, nil
}

// upstreamsOf is each spec as the proxy's upstream, the built-in on
// builtinURL and bearer.
func upstreamsOf(specs map[string]tools.UpstreamSpec, builtinURL string, bearer TokenSource) map[string]sessionproxy.Upstream {
	ups := make(map[string]sessionproxy.Upstream, len(specs))
	for name, sp := range specs {
		switch {
		case name == tools.Builtin:
			ups[name] = sessionproxy.Runsheet{URL: builtinURL, Bearer: bearer}
		case sp.Transport == tools.Local:
			ups[name] = sessionproxy.Local{Command: sp.Command, Args: sp.Args, Env: sp.Env, Secrets: sp.Secrets}
		default:
			ups[name] = sessionproxy.Remote{URL: sp.URL, Headers: sp.Headers}
		}
	}
	return ups
}

// Adopt starts the proxy of a session another node process started
// (mesh-cli ADR-009): on the socket its state names — the path the
// agent's MCP entries still dial, a stale socket left by the node that
// died removed first (sessionproxy.Listen) — fronting the servers the
// state names, each upstream rendered again from use, the session's
// tools as the claim-time read hands them to this node, and the
// built-in's bearer from Bearer: for a build its own mesh session's,
// re-bound by the adopter before this (ADR-035). A server the state
// names that use no longer has is left out and named in missing: the
// proxy answers it as no MCP server. Nil and no error when the state
// names no proxy.
func (px *SessionProxies) Adopt(st sessionunit.State, builtin map[string]any, use []tools.Tool) (sp *SessionProxy, missing []string, err error) {
	if st.Proxy == nil || st.Proxy.Socket == "" {
		return nil, nil, nil
	}
	if px == nil || px.Bearer == nil {
		return nil, nil, errors.New("sessionproxy: no bearer source")
	}
	_, url, ok := harness.RelayOf(builtin[tools.Builtin])
	if !ok {
		return nil, nil, errors.New("sessionproxy: no runsheet relay to proxy")
	}
	_, specs, err := tools.ProxiedServers(url, use, func(string) map[string]any { return nil })
	if err != nil {
		return nil, nil, err
	}
	named := make(map[string]tools.UpstreamSpec, len(st.Proxy.Servers))
	for _, name := range st.Proxy.Servers {
		spec, ok := specs[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		named[name] = spec
	}
	meta := harness.SessionMeta{NodeID: st.NodeID, ItemID: st.ItemID, GoalID: st.GoalID, DirectionID: st.DirectionID, Attempt: st.Attempt}
	bearer, err := px.Bearer(harness.Kind(st.Kind), meta)
	if err != nil {
		return nil, missing, err
	}
	dir := filepath.Dir(st.Proxy.Socket)
	// a host session's path only needs to exist again; a pod's mount is
	// the directory the old node kept (SessionProxy.Stop)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, missing, fmt.Errorf("sessionproxy: %w", err)
	}
	p := sessionproxy.New(upstreamsOf(named, url, bearer))
	p.Logf = px.Logf
	if err := p.Listen(st.Proxy.Socket); err != nil {
		return nil, missing, fmt.Errorf("sessionproxy: listen %s: %w", st.Proxy.Socket, err)
	}
	return &SessionProxy{Dir: dir, Socket: st.Proxy.Socket, p: p}, missing, nil
}

// UseVia is UseTools through px: with a proxy, every server — the
// built-in, whose relay names its gateway URL and the mesh binary, and
// each tool in use — becomes a harness.ProxyRelay entry on a socket px
// starts for the session under root (its org's MCP root, else px.Root),
// the job carrying no tool secret in its env and no tool for Codex to
// render on its own; acct remembers the tools and the proxy's errors
// for UseNote. The returned end stops the proxy at the session's end
// (SessionProxy.End: its directory kept when ctx is done, the session
// detached for the node that adopts it). With no px, or a built-in that
// is no relay, it is UseTools and end does nothing.
func (st SessionTools) UseVia(px *SessionProxies, root string, job *harness.Job, builtin map[string]any, acct *SessionAccount) (func(context.Context), error) {
	noop := func(context.Context) {}
	bin, url, ok := harness.RelayOf(builtin[tools.Builtin])
	if px == nil || !ok {
		return noop, st.UseTools(job, builtin, acct)
	}
	var sp *SessionProxy
	shim := func(name string) map[string]any { return harness.ProxyRelay(bin, sp.Socket, name) }
	// the specs first, so a refused tool set starts no proxy
	_, specs, err := tools.ProxiedServers(url, st.Use, func(string) map[string]any { return nil })
	if err != nil {
		return noop, err
	}
	if sp, err = px.open(job, root, url, specs); err != nil {
		return noop, err
	}
	servers, _, err := tools.ProxiedServers(url, st.Use, shim)
	if err != nil {
		sp.Close()
		return noop, err
	}
	job.MCPServers = servers
	job.Tools = nil
	// the values stay redacted from what the session prints
	for _, spec := range specs {
		for _, v := range spec.Headers {
			job.Secrets = append(job.Secrets, v)
		}
		for _, v := range spec.Secrets {
			job.Secrets = append(job.Secrets, v)
		}
	}
	if acct != nil {
		acct.used, acct.toolEnv, acct.proxy = st.Use, nil, sp
	}
	return sp.End, nil
}

// adoptProxy restarts an adopted session's proxy (SessionProxies.Adopt)
// on the tools the claim-time read hands this node for ref, selected for
// role and runtimes as the session's start selected them, and answers
// the proxy's end, its directory kept should this node detach in turn. A
// session whose state names no proxy, or a node with none, restarts
// nothing. A failure is logged: the session runs on, its MCP calls
// unanswered, rather than its build being lost.
func adoptProxy(ctx context.Context, px *SessionProxies, creds *Credentials, builtin map[string]any, role string, runtimes []string, ref gateway.SessionRef, s FoundSession, label string, logf func(string, ...any)) func(context.Context) {
	noop := func(context.Context) {}
	if s.State.Proxy == nil {
		return noop
	}
	if px == nil {
		logf("%s: adopted session names a proxy at %s and this node runs none; its MCP servers go unanswered", label, s.State.Proxy.Socket)
		return noop
	}
	var use []tools.Tool
	if creds != nil && creds.GW != nil {
		read, err := creds.GW.Session(ctx, ref, s.State.NodeID)
		switch {
		case err != nil && !gateway.IsUnsupported(err):
			logf("%s: adopted proxy: tools: %v; proxying runsheet alone", label, err)
		case read != nil:
			use = SelectTools(builtin, read.Tools, role, runtimes).Use
		}
	}
	sp, missing, err := px.Adopt(s.State, builtin, use)
	if err != nil {
		logf("%s: adopted proxy: %v", label, err)
		return noop
	}
	line := fmt.Sprintf("%s: proxy restarted on %s (%s)", label, sp.Socket, strings.Join(s.State.Proxy.Servers, ", "))
	if len(missing) > 0 {
		line += "; no longer served: " + strings.Join(missing, ", ")
	}
	logf("%s", line)
	return sp.End
}
