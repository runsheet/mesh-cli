package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
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
	if s.p != nil {
		_ = s.p.Close()
	}
	if s.Dir != "" {
		os.RemoveAll(s.Dir)
	}
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
	p := sessionproxy.New(ups)
	p.Logf = px.Logf
	sock := filepath.Join(dir, ProxySocketName)
	if err := p.Listen(sock); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("sessionproxy: listen %s: %w", sock, err)
	}
	return &SessionProxy{Dir: dir, Socket: sock, p: p}, nil
}

// UseVia is UseTools through px: with a proxy, every server — the
// built-in, whose relay names its gateway URL and the mesh binary, and
// each tool in use — becomes a harness.ProxyRelay entry on a socket px
// starts for the session under root (its org's MCP root, else px.Root),
// the job carrying no tool secret in its env and no tool for Codex to
// render on its own; acct remembers the tools and the proxy's errors
// for UseNote. The returned close stops the proxy at the session's end.
// With no px, or a built-in that is no relay, it is UseTools and close
// does nothing.
func (st SessionTools) UseVia(px *SessionProxies, root string, job *harness.Job, builtin map[string]any, acct *SessionAccount) (func(), error) {
	noop := func() {}
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
	return sp.Close, nil
}

// mcpRootFor is the MCP root of the org whose id is orgID when roots
// are laid out, else "" (SessionProxies.Root).
func mcpRootFor(roots *OrgRootSet, scopes Scopes, orgID string) string {
	if roots == nil {
		return ""
	}
	r, err := roots.ForOrgID(scopes, orgID)
	if err != nil {
		return ""
	}
	return r.MCP
}

// mcpRootOfScope is mcpRootFor by the session's scope.
func mcpRootOfScope(roots *OrgRootSet, scope Scope) string {
	if roots == nil {
		return ""
	}
	r, err := roots.For(scope.Name())
	if err != nil {
		return ""
	}
	return r.MCP
}
