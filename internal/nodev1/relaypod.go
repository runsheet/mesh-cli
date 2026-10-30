package nodev1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

// A sandboxed lease's relay as a sidecar in its pod (projectmesh-io/api
// ADR 001 amendment "relay sidecar"; `mesh node run --sandbox
// --relay-sidecar`). The node daemon holds the machine credential, the
// model account key and every live lease's session; the relay reads
// what the pod sends it. With RelaySidecar set, each lease's relay runs
// as `mesh relay-sidecar` in a container of the lease's own pod, under a
// UID of its own, on the lease's credentials alone:
//
//   - the pod has no network (sandbox.Isolated) and shares its network
//     namespace alone, so the agent reaches nothing but its loopback,
//     where the sidecar serves /git/, the product MCP, /latchkey/,
//     /model/ and CONNECT;
//   - the sidecar reaches everything through one socket to the node, the
//     lease's socket directory mounted into the sidecar alone, where the
//     node serves the egress tunnel (internal/egress: the kind's hosts,
//     or any host by HTTPS when the kind declares no egress, and the
//     fixed upstreams), the model forwarder, the node tools and the Stop
//     hook's gate;
//   - the node hands the sidecar its credentials once, through `podman
//     exec -i` into the sidecar's own tmpfs, and renews them on asking;
//   - the push tool asks the sidecar to push, so every push is checked in
//     one place; the node's own grant, for the checkout and apply_onto's
//     fetches, is read-only.
//
// Ending the lease closes the socket and its tunnels, revokes every git
// token the node minted for the sidecar, removes the socket directory,
// and removes the pod with the sidecar and its tmpfs; Mesh revokes the
// Latchkey session as before.

// Names in a lease's socket directory, as the sidecar has it mounted at
// sandbox.RelaySocketDir.
const (
	// relayNodeSocket is the node's side of the lease's socket.
	relayNodeSocket = "n"
	// relayControlDir holds the sidecar's control socket: the one
	// directory the sidecar's user may write.
	relayControlDir = "r"
	// relayControlSocket is the sidecar's control socket in it.
	relayControlSocket = "s"
)

// RelayURLEnv names the relay sidecar's loopback root in the session's
// environment, where /latchkey/<vendor>/ is reached.
const RelayURLEnv = "MESH_RELAY_URL"

// relayReadyWait bounds the wait for the sidecar's control socket.
const relayReadyWait = 60 * time.Second

// sidecarMode says the node runs each lease's relay as a sidecar.
func (r *Runtime) sidecarMode() bool { return r.Pods != nil && r.RelaySidecar }

// relaySidecar is one lease's node side of its relay sidecar.
type relaySidecar struct {
	r      *Runtime
	b      *meshapi.Brief
	lp     *leasePod
	dir    string
	tokens *sessionTokens
	// gitReq is the sidecar's git token mint: the brief's own access.
	gitReq SourceRequest
	node   *relaysidecar.Node
	tunnel *egress.Tunnel
	// own is the node's own product proxy, on a socket only the node
	// has: what hands in a finish's learnings (callProductTool).
	own     *sessionproxy.Proxy
	ownDir  string
	ownSock string

	mu     sync.Mutex
	minted []gitrelay.Token
	closed bool
}

// relayBase is the sidecar's loopback root in the pod.
func (rs *relaySidecar) base() string { return rs.lp.bridgeBase() }

// controlSocket is the sidecar's control socket on the host.
func (rs *relaySidecar) controlSocket() string {
	return filepath.Join(rs.dir, relayControlDir, relayControlSocket)
}

// prepareRelaySidecar makes the lease's socket directory for its
// sidecar: 0711, so the sidecar's user reaches the sockets in it by
// name and lists nothing, under a root only the node's user enters; and
// the control directory, the sidecar's user's own.
func (r *Runtime) prepareRelaySidecar(ctx context.Context, b *meshapi.Brief, lp *leasePod, tokens *sessionTokens) (*relaySidecar, error) {
	dir, err := r.leaseSocketDir(b.LeaseID)
	if err != nil {
		return nil, err
	}
	rs := &relaySidecar{r: r, b: b, lp: lp, dir: dir, tokens: tokens}
	if err := os.Chmod(dir, 0o711); err != nil {
		rs.removeDir(ctx)
		return nil, err
	}
	ctl := filepath.Join(dir, relayControlDir)
	if err := os.Mkdir(ctl, 0o711); err != nil {
		rs.removeDir(ctx)
		return nil, err
	}
	if err := lp.sess.ChownForRelay(ctx, ctl); err != nil {
		rs.removeDir(ctx)
		return nil, fmt.Errorf("giving the relay sidecar its control directory: %w", err)
	}
	return rs, nil
}

// pusher is the push tool's way to the sidecar.
func (rs *relaySidecar) pusher() nodetools.Pusher {
	pc := relaysidecar.PushClient{Socket: rs.controlSocket()}
	return func(ctx context.Context, ref, sha string) (nodetools.PushOutcome, error) {
		res, err := pc.Push(ctx, ref, sha)
		if err != nil {
			return nodetools.PushOutcome{}, err
		}
		return nodetools.PushOutcome{Pushed: res.Pushed, Refused: res.Refused, Failed: res.Failed}, nil
	}
}

// open serves the node's side of the lease's socket, starts the sidecar
// in the pod and hands it its credentials, and answers the session's MCP
// servers as the pod reaches them (each through the proxy shim, on the
// sidecar's loopback). workspace is the lease's checkout, which the
// sidecar mounts read-only to prove a push's fast-forward.
func (rs *relaySidecar) open(ctx context.Context, tools []mcptool.Tool, observe func(string, []byte, []byte), gate *stopGate, account *sessionAccount, sl *slots, workspace string) (map[string]any, error) {
	r, b := rs.r, rs.b
	// the node's side: node tools, the model forwarder, the stop gate,
	// renewal, AccessRecords, the tunnel
	np := sessionproxy.New(nil)
	np.Logf = r.Logf
	if len(tools) > 0 {
		np.Add(NodeToolsServer, sessionproxy.InProcess{Handler: &mcptool.Handler{Name: NodeToolsServer, Tools: tools}})
	}
	models := r.ModelUpstreams
	if models == nil {
		models = modelUpstreams()
	}
	account.forward(np, models, r.logf, b.LeaseID, rs.base())
	// egress.model never closes the harness's own model route (#1700)
	fixed, err := rs.fixedUpstreams()
	if err != nil {
		return nil, err
	}
	var kind []string
	if b.Egress != nil {
		kind = b.Egress.Hosts
	}
	policy := egress.NewPolicy(kind, tenantHosts(b), fixed)
	if b.Egress == nil {
		// a kind that declares no egress keeps the internet by HTTPS,
		// now only through the tunnel
		policy = policy.Open()
	}
	// every request of the sidecar's tunnel is reported, as the
	// in-process relay's (security.go)
	report := r.egressReporter(b.LeaseID)
	audit := r.tunnelAudit(b.LeaseID, "the relay sidecar's tunnel", report)
	rs.tunnel = &egress.Tunnel{
		Policy: policy, Dial: r.EgressDial, Logf: r.logf,
		Audit: func(c egress.Call) {
			if !c.Allowed {
				audit(c)
			}
		},
		Done: report,
	}
	rs.node = &relaysidecar.Node{
		Tunnel: rs.tunnel, Proxy: np, Renew: rs.renew, Logf: r.logf,
		Access:  func(rec meshapi.AccessRecord) { r.keepAccess(b.LeaseID, rec) },
		Observe: observe,
		// what the sidecar itself refused, which the node posts: the
		// sidecar holds no machine token
		Security: r.securityReporter(b.LeaseID),
	}
	if gate != nil {
		rs.node.Stop = gate.handler()
	}
	nodeSock := filepath.Join(rs.dir, relayNodeSocket)
	if err := rs.node.Listen(nodeSock); err != nil {
		return nil, err
	}
	creds, err := rs.credentials(ctx, sl)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		return nil, err
	}
	bin, err := relayBinary(r.MeshBinary)
	if err != nil {
		return nil, err
	}
	// the sidecar's user reads the checkout to prove a fast-forward:
	// its root opened to others, as the lease's directory above it is
	// not
	if err := os.Chmod(workspace, 0o755); err != nil {
		return nil, err
	}
	spec := sandbox.RelaySpec{
		Binary: bin, SocketDir: rs.dir, Checkout: workspace,
		Args: []string{relaysidecar.Command,
			"--credentials-dir", sandbox.RelayCredentialsDir,
			"--node", sandbox.RelaySocketDir + "/" + relayNodeSocket,
			"--control", sandbox.RelaySocketDir + "/" + relayControlDir + "/" + relayControlSocket,
			"--listen", "127.0.0.1:" + strconv.Itoa(rs.lp.gitPort)},
	}
	if err := rs.lp.sess.StartRelay(ctx, spec); err != nil {
		return nil, err
	}
	if err := rs.lp.sess.InstallRelay(ctx, bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	if err := waitListening(ctx, rs.controlSocket(), relayReadyWait); err != nil {
		return nil, fmt.Errorf("the relay sidecar did not start: %w", err)
	}
	if err := rs.openOwn(ctx); err != nil {
		return nil, err
	}
	// the session's servers, each on the sidecar's loopback through the
	// node image's proxy shim
	servers := map[string]any{}
	for _, s := range creds.Servers {
		servers[s.Name] = harness.ProxyRelay(r.MeshBinary, rs.base(), s.Name)
	}
	if len(tools) > 0 {
		servers[NodeToolsServer] = harness.ProxyRelay(r.MeshBinary, rs.base(), NodeToolsServer)
	}
	return rs.lp.podServers(servers, "")
}

// credentials are what the sidecar holds: the lease's alone.
func (rs *relaySidecar) credentials(ctx context.Context, sl *slots) (relaysidecar.Credentials, error) {
	b, tokens := rs.b, rs.tokens
	c := relaysidecar.Credentials{Lease: b.LeaseID, API: apiRules(b), Tools: sl.filter()}
	if b.Registry != nil {
		// the kind's registry, which the sidecar serves on the pod's
		// loopback (registry.go)
		c.Registry = b.Registry.BaseURL
	}
	if tokens == nil {
		return c, errors.New("the lease has no Latchkey session for its relay sidecar")
	}
	h, err := tokens.Key().Handoff()
	if err != nil {
		return c, err
	}
	c.Key = h
	tok, exp, err := tokens.current(ctx)
	if err != nil {
		return c, credentialFailed(err)
	}
	c.Session = relaysidecar.Token{Value: tok, ExpiresAt: exp}
	if len(c.API) > 0 {
		if c.Latchkey, err = tokens.issuer(); err != nil {
			return c, err
		}
	}
	for _, s := range b.Servers {
		if s.Name == "" || s.URL == "" || s.Name == NodeToolsServer || !mcpAllowed(b, s.Name) {
			continue
		}
		c.Servers = append(c.Servers, relaysidecar.Server{Name: s.Name, URL: s.URL})
	}
	for _, t := range []string{b.FinishTool, b.LearningsTool} {
		if t == "" {
			continue
		}
		if i := strings.LastIndex(t, "__"); i >= 0 && strings.HasPrefix(t, "mcp__") {
			t = t[i+2:]
		}
		c.Watch = append(c.Watch, t)
	}
	if b.Git != nil && b.Git.Repo != "" {
		policy := policyOf(b.Git)
		rs.gitReq = sourceRequest(b.Git, policy)
		t, err := rs.mintGit(ctx)
		if err != nil {
			return c, credentialFailed(fmt.Errorf("the relay sidecar's git token: %w", err))
		}
		c.Git = &relaysidecar.Git{Policy: policy, Upstream: rs.r.GitUpstream, Token: t, Checkout: sandbox.RelayCheckout}
	}
	return c, nil
}

// mintGit mints a git token for the sidecar on the lease's session, and
// keeps it to revoke when the lease ends.
func (rs *relaySidecar) mintGit(ctx context.Context) (relaysidecar.Token, error) {
	t, err := rs.tokens.MintSource(ctx, rs.gitReq)
	if err != nil {
		return relaysidecar.Token{}, err
	}
	rs.mu.Lock()
	rs.minted = append(rs.minted, t)
	rs.mu.Unlock()
	return relaysidecar.Token{Value: t.Value, ExpiresAt: t.ExpiresAt}, nil
}

// renew is the sidecar's renewal: the lease's session token, and a git
// token when it asks; nothing once the lease has ended.
func (rs *relaySidecar) renew(ctx context.Context, req relaysidecar.RenewRequest) (relaysidecar.Renewal, error) {
	rs.mu.Lock()
	closed := rs.closed
	rs.mu.Unlock()
	if closed {
		return relaysidecar.Renewal{}, errors.New("the lease has ended")
	}
	tok, exp, err := rs.tokens.current(ctx)
	if err != nil {
		return relaysidecar.Renewal{}, err
	}
	out := relaysidecar.Renewal{Session: relaysidecar.Token{Value: tok, ExpiresAt: exp}}
	if req.Git {
		if rs.gitReq.Repository == "" {
			return relaysidecar.Renewal{}, errors.New("the lease has no git")
		}
		t, err := rs.mintGit(ctx)
		if err != nil {
			return relaysidecar.Renewal{}, err
		}
		out.Git = &t
	}
	return out, nil
}

// fixedUpstreams are the hosts the sidecar reaches itself: Latchkey,
// the product's MCP servers and the git vendor, as host or host:port.
func (rs *relaySidecar) fixedUpstreams() ([]string, error) {
	b := rs.b
	var out []string
	add := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return fmt.Errorf("%q is not an upstream url", raw)
		}
		out = append(out, u.Host)
		return nil
	}
	if rs.tokens != nil {
		if iss, err := rs.tokens.issuer(); err == nil {
			_ = add(iss)
		}
	}
	for _, s := range b.Servers {
		if s.URL != "" && mcpAllowed(b, s.Name) {
			if err := add(s.URL); err != nil {
				return nil, err
			}
		}
	}
	if h := registryHost(b); h != "" {
		out = append(out, h)
	}
	if b.Git != nil && b.Git.Repo != "" {
		policy := policyOf(b.Git)
		origin := policy.Base
		if rs.r.GitUpstream != "" {
			origin = rs.r.GitUpstream
		}
		if origin == "" {
			v, err := gitrelay.VendorNamed(policy.Vendor)
			if err != nil {
				return nil, err
			}
			origin = v.Origin()
		}
		if err := add(origin); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// openOwn serves the node's own product proxy on a socket of a
// directory only the node has: what hands in a finish's learnings after
// the session, on the node's own network, not the pod's.
func (rs *relaySidecar) openOwn(ctx context.Context) error {
	ups := map[string]sessionproxy.Upstream{}
	for _, s := range rs.b.Servers {
		if s.Name != "" && s.URL != "" && s.Name != NodeToolsServer {
			ups[s.Name] = sessionproxy.Product{URL: s.URL, Bearer: rs.tokens.Token, Key: rs.tokens.Key()}
		}
	}
	if len(ups) == 0 {
		return nil
	}
	dir, err := rs.r.leaseSocketDir(rs.b.LeaseID + "\x00own")
	if err != nil {
		return err
	}
	p := sessionproxy.New(ups)
	p.Logf = rs.r.Logf
	sock := filepath.Join(dir, leaseSocketName)
	if err := p.Listen(sock); err != nil {
		os.RemoveAll(dir)
		return err
	}
	if err := waitListening(ctx, sock, socketReadyWait); err != nil {
		p.Close()
		os.RemoveAll(dir)
		return err
	}
	rs.own, rs.ownDir, rs.ownSock = p, dir, sock
	return nil
}

// close ends the node's side: the socket and its tunnels, the node's own
// proxy, every git token minted for the sidecar, and the socket
// directory. The sidecar and its tmpfs go with the pod.
func (rs *relaySidecar) close() error {
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return nil
	}
	rs.closed = true
	minted := rs.minted
	rs.mu.Unlock()
	if rs.node != nil {
		_ = rs.node.Close()
	}
	if rs.own != nil {
		rs.own.Close()
		os.RemoveAll(rs.ownDir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	if len(minted) > 0 && rs.tokens != nil {
		if err := rs.tokens.RevokeSource(ctx, minted); err != nil {
			rs.r.logf("nodev1: lease %s: revoking the relay sidecar's git tokens: %v", rs.b.LeaseID, err)
		}
	}
	rs.removeDir(ctx)
	return nil
}

// removeDir removes the socket directory, the sidecar's control
// directory in it as the pod's user namespace owns it.
func (rs *relaySidecar) removeDir(ctx context.Context) {
	if err := os.RemoveAll(rs.dir); err == nil {
		return
	}
	if err := rs.lp.sess.RemoveTree(ctx, rs.dir); err != nil {
		rs.r.logf("nodev1: lease %s: removing the relay's socket directory: %v", rs.b.LeaseID, err)
	}
}

// relayBinary is the node's own executable, which the sidecar runs.
func relayBinary(meshBinary string) (string, error) {
	if meshBinary != "" && filepath.IsAbs(meshBinary) {
		if _, err := os.Stat(meshBinary); err == nil {
			return meshBinary, nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("the node's own binary, for the relay sidecar: %w", err)
	}
	return filepath.EvalSymlinks(exe)
}

// relayPrompt is the prompt's vendor API line for a session whose relay
// is a sidecar: HTTP on its loopback.
func relayPrompt(b *meshapi.Brief, p string) string {
	rules := apiRules(b)
	if len(rules) == 0 {
		return p
	}
	var sb strings.Builder
	sb.WriteString(p)
	sb.WriteString("\nVendor API calls (pull requests, comments, merges, statuses) go only through the node's relay: HTTP to ")
	fmt.Fprintf(&sb, "$%s/latchkey/<vendor>/<api path> (for example `curl \"$%s/latchkey/%s/<path>\"`). ", RelayURLEnv, RelayURLEnv, rules[0].Vendor)
	sb.WriteString("The relay adds the credential; send none. This job may call:\n")
	for _, r := range rules {
		fmt.Fprintf(&sb, "- %s: %s %s\n", r.Vendor, strings.Join(r.Methods, ", "), strings.Join(r.Paths, ", "))
	}
	return sb.String()
}
