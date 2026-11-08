package nodev1

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// postgresBasePort is the first loopback port, inside a lease's pod,
// of its tcp_auth bridges (podGitPort's neighbor); each further
// tcp_auth credential of the same lease takes the next one.
const postgresBasePort = 47420

// RelayURLEnv names the relay sidecar's loopback root in the session's
// environment, where /latchkey/<vendor>/ is reached.
const RelayURLEnv = "MESH_RELAY_URL"

// relayReadyWait bounds the wait for the sidecar's control socket.
const relayReadyWait = 60 * time.Second

// Names in the lease's egress process's own private socket directory
// (ADR-051 decision 1): reached by the node's UID alone, never the
// pod's — the process's control socket, and its own node and lane
// sockets, which the node's pod-facing relayNodeSocket and
// relayLaneSocket forward every CONNECT onto unchanged.
const (
	egressControlSocket = "control"
	egressNodeSocket    = "node"
	egressLaneSocket    = "lane"
)

// TestEgressDialEnv, set in the lease-egress process's own environment,
// redirects every dial it makes to the named address instead of the
// real host. A test's own Runtime.EgressDial cannot reach a lease's
// egress once it runs as a process of its own (ADR-051): this is its
// equivalent across the process boundary, read only by the
// lease-egress subcommand and never set outside a test.
const TestEgressDialEnv = "MESH_LEASE_EGRESS_TEST_DIAL"

// TestEgressUpstreamServerNameEnv, set alongside TestEgressDialEnv, is
// the TLS ServerName a test's redirected dial must verify as instead of
// the request's own host: a test server's certificate covers a name of
// its own (example.com), never the real host (api.runsheet.dev) a
// terminated request is addressed to. A test's own Runtime.EgressUpstream
// carries the same override to the in-process tunnel's transport; this
// is its equivalent for a lease's egress once it runs as a process of
// its own (ADR-051), read only by the lease-egress subcommand and never
// set outside a test.
const TestEgressUpstreamServerNameEnv = "MESH_LEASE_EGRESS_TEST_UPSTREAM_SERVERNAME"

// TestEgressUpstreamCAEnv, set alongside TestEgressUpstreamServerNameEnv,
// is a PEM-encoded certificate the redirected dial's TLS verification
// must trust in place of the system roots: a test server's certificate
// is its own, signed by no root the subprocess otherwise holds. Mirrors
// what a test's own Runtime.EgressUpstream trusts in-process by cloning
// the test server's own client transport.
const TestEgressUpstreamCAEnv = "MESH_LEASE_EGRESS_TEST_UPSTREAM_CA"

// egressStopWait bounds the wait for the lease's egress process to
// exit after SIGTERM before it is killed.
const egressStopWait = 5 * time.Second

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
	// pushHeads are where the job found its push_to, the sidecar's
	// relay's heads for a leased push (gitrelay/lease.go)
	pushHeads map[string]string
	// ic is what the tunnel terminates for the session (intercept.go):
	// the sidecar's own connections are spliced whatever it names
	ic *interception
	// own is the node's own product proxy, on a socket only the node
	// has: what hands in a finish's learnings (callProductTool).
	own     *sessionproxy.Proxy
	ownDir  string
	ownSock string

	// mirrors are the node's mirrors this lease's sidecar may read
	// (relaymirrors.go), mirror the one of its repository and
	// readMirrors its read repos', by repository.
	mirrors     []string
	mirror      string
	readMirrors map[string]string

	// lanes serves the lease's egress to run_suite's lanes
	// (openLaneTunnel)
	lanes *http.Server

	// laneHandler is the lane's inner handler when the lease's egress
	// runs as a process of its own (ADR-051): forwardConnect onto its
	// lane socket. nil when the tunnel is in process (rs.tunnel, above,
	// serves the lane directly).
	laneHandler http.Handler
	// caPEM is the CA the lease's egress process minted for the rules it
	// terminates (LeaseEgressStartResult), when the lease's egress runs
	// as a process of its own: the pod trusts it the same way it would
	// trust one minted in process (intercept.go's trust), since the CA
	// itself never leaves that process.
	caPEM []byte
	// egressCmd is the lease's egress process (ADR-051 decision 1): its
	// own process group, outside the node's and the pod's, ended by
	// stopEgress. nil when the lease's egress runs in process.
	egressCmd *exec.Cmd
	// egressDir is that process's own private socket directory
	// (control, node, lane), reached by the node's UID alone.
	egressDir string
	// egressAuditCancel stops the goroutine reading that process's
	// audit stream (ADR-051 decision 2).
	egressAuditCancel context.CancelFunc
	// egressDone closes when egressCmd's single Wait goroutine sees it
	// exit (started once, right after it starts): stopEgress and
	// watchEgress both read it, so cmd.Wait is never called twice.
	egressDone chan struct{}
	// egressStopping is set before stopEgress signals the process, so
	// its own exit is not read as an unexpected death.
	egressStopping bool

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
	return func(ctx context.Context, ref, sha, lease string) (nodetools.PushOutcome, error) {
		res, err := pc.Push(ctx, ref, sha, lease)
		if err != nil {
			return nodetools.PushOutcome{}, err
		}
		return nodetools.PushOutcome{Pushed: res.Pushed, Refused: res.Refused, Failed: res.Failed, Lease: res.Lease}, nil
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
	// the node tools' calls, which the sidecar forwards unchanged
	np.Canary = r.injectedOf(b.LeaseID).canary()
	if len(tools) > 0 {
		np.Add(NodeToolsServer, sessionproxy.InProcess{Handler: &mcptool.Handler{Name: NodeToolsServer, Tools: tools}})
	}
	models := r.ModelUpstreams
	if models == nil {
		models = modelUpstreams()
	}
	if err := account.forward(np, models, r.logf, b.LeaseID, rs.base()); err != nil {
		return nil, err
	}
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
	rawUps, err := rs.rawUpstreams()
	if err != nil {
		return nil, err
	}
	for _, hp := range rawUps {
		if err := policy.AddRaw(hp); err != nil {
			return nil, err
		}
	}
	// every request of the sidecar's tunnel is reported, as the
	// in-process relay's (security.go)
	report := r.egressReporter(b.LeaseID)
	audit := r.tunnelAudit(b.LeaseID, "the relay sidecar's tunnel", report)
	// a lease whose relay runs as a process of its own (ADR-051) builds
	// no tunnel here: that process keeps its own CA and rules, driven
	// over its control socket below, once the pod-facing sockets are
	// open. injects and federates stay entirely in process (ADR-051
	// decision 4, a later goal): InjectRule is not representable over
	// the wire yet.
	outOfProcess := r.sidecarMode() && rs.lp != nil && !injects(b) && !federates(b)
	if !outOfProcess {
		rs.tunnel = &egress.Tunnel{
			Policy: policy, Dial: r.EgressDial, Logf: r.logf,
			Audit: func(c egress.Call) {
				if !c.Allowed {
					audit(c)
				}
			},
			Done: report,
		}
		rs.ic.install(r, b.LeaseID, rs.tunnel)
	}
	rs.node = &relaysidecar.Node{
		Tunnel: rs.tunnel, Proxy: np, Renew: rs.renew, Logf: r.logf,
		Access:  func(rec meshapi.AccessRecord) { r.keepAccess(b.LeaseID, rec) },
		Observe: observe,
		// what the sidecar itself refused, which the node posts: the
		// sidecar holds no machine token
		Security: r.securityReporter(b.LeaseID),
		// the sidecar's MCP and vendor API calls, scanned and redacted
		// here, where the lease's injected values are (canary.go)
		Canary: r.injectedOf(b.LeaseID).canary(),
	}
	if gate != nil {
		rs.node.Stop = gate.handler()
	}
	nodeSock := filepath.Join(rs.dir, relayNodeSocket)
	if err := rs.node.Listen(nodeSock); err != nil {
		return nil, err
	}
	// the node's mirrors the sidecar may read, before its credentials
	// name the repository's (relaymirrors.go)
	rs.mirror, rs.readMirrors, rs.mirrors = r.leaseMirrors(b)
	creds, err := rs.credentials(ctx, sl)
	if err != nil {
		return nil, err
	}
	if outOfProcess {
		if err := rs.startEgressProcess(ctx, policy, b, creds, report, audit); err != nil {
			return nil, fmt.Errorf("the lease's egress process: %w", err)
		}
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
		Binary: bin, SocketDir: rs.dir, Checkout: workspace, Mirrors: rs.mirrors,
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
	// the kind's filled tool slots, each a server of the sidecar's
	for _, s := range creds.Slots {
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
	c := relaysidecar.Credentials{Lease: b.LeaseID, API: apiRules(b), Tools: sl.filter(), Slots: sl.sidecarSlots(),
		// the node scans and redacts the sidecar's calls for the
		// lease's injected values (canary.go)
		Canary: rs.r.injectedOf(b.LeaseID) != nil && carriesCredentials(b)}
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
	if len(c.API) > 0 || len(c.Slots) > 0 {
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
		policy.Heads = rs.pushHeads
		rs.gitReq = sourceRequest(b.Git, policy)
		t, err := rs.mintGit(ctx, rs.gitReq)
		if err != nil {
			return c, credentialFailed(fmt.Errorf("the relay sidecar's git token: %w", err))
		}
		// a read repo's token is minted when the sidecar first fetches
		// it (renew)
		c.Git = &relaysidecar.Git{Policy: policy, Upstream: rs.r.GitUpstream, Token: t, Checkout: sandbox.RelayCheckout,
			Mirror: rs.mirror, ReadMirrors: rs.readMirrors}
	}
	if rs.ic != nil {
		port := postgresBasePort
		for _, h := range rs.ic.held {
			if h.Spec().FormOf() != egress.FormTCPAuth {
				continue
			}
			cred, err := h.Credential(ctx)
			if err != nil {
				return c, credentialFailed(fmt.Errorf("the relay sidecar's postgres credential: %w", err))
			}
			user, pass, database, err := cred.TCPAuth()
			if err != nil {
				return c, credentialFailed(fmt.Errorf("the relay sidecar's postgres credential: %w", err))
			}
			spec := h.Spec()
			plaintext, rootCA := cred.TCPAuthTLS()
			if plaintext && !relaysidecar.PlaintextAllowed(spec.Host) {
				return c, credentialFailed(fmt.Errorf("the relay sidecar's postgres credential %s: sslmode disable sends its password without TLS, which only a loopback upstream may have, not %s", h.ref, spec.Host))
			}
			c.Postgres = append(c.Postgres, relaysidecar.Postgres{
				Listen: port, Host: spec.Host, Port: spec.Port,
				User: user, Password: pass, Database: database,
				RootCA: rootCA, Plaintext: plaintext,
			})
			port++
		}
	}
	return c, nil
}

// mintGit mints a git token for the sidecar on the lease's session, and
// keeps it to revoke when the lease ends.
func (rs *relaySidecar) mintGit(ctx context.Context, req SourceRequest) (relaysidecar.Token, error) {
	t, err := rs.tokens.MintSource(ctx, req)
	if err != nil {
		return relaysidecar.Token{}, err
	}
	// redacted and a canary before the sidecar first sends it
	if rs.r != nil && rs.b != nil {
		rs.r.injectedOf(rs.b.LeaseID).addGitToken(rs.b.Git, req.Repository, t)
	}
	rs.mu.Lock()
	rs.minted = append(rs.minted, t)
	rs.mu.Unlock()
	return relaysidecar.Token{Value: t.Value, ExpiresAt: t.ExpiresAt}, nil
}

// renew is the sidecar's renewal: the lease's session token, and a git
// token when it asks — the job's own repository's at the brief's access,
// or, when it names one of the brief's read_repos, a read-only token for
// that repository alone; a repository the brief does not name is
// refused. Nothing once the lease has ended.
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
		mint, err := rs.gitRequestFor(req.Repo)
		if err != nil {
			return relaysidecar.Renewal{}, err
		}
		t, err := rs.mintGit(ctx, mint)
		if err != nil {
			return relaysidecar.Renewal{}, err
		}
		out.Git = &t
	}
	return out, nil
}

// gitRequestFor is the mint a renewal of repo's git token asks for:
// the job's own when repo is "" or names it, a read token for one of
// the brief's read_repos, as the brief names it, and nothing else.
func (rs *relaySidecar) gitRequestFor(repo string) (SourceRequest, error) {
	if repo == "" || strings.EqualFold(repo, rs.gitReq.Repository) {
		return rs.gitReq, nil
	}
	for _, r := range rs.b.Git.ReadRepos {
		if strings.EqualFold(repo, r) {
			return sourceRequestFor(rs.b.Git, r, gitrelay.Read), nil
		}
	}
	return SourceRequest{}, fmt.Errorf("the brief names no read repo %s", repo)
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

// rawUpstreams are a tcp_auth credential's own host:port — the
// sidecar's own dial to the real database, never the session's: no
// InjectRule names it, so the tunnel pipes it (Policy.AddRaw) for the
// sidecar's own CONNECT alone, with no TLS ClientHello asked for, since
// what rides it is the Postgres wire protocol (its TLS the sidecar's).
// Its host must be one of the kind's egress hosts (narrowed by the
// tenant's): Latchkey's answer never widens where a lease reaches, so a
// credential naming any other host, or a kind with no egress, fails the
// lease.
func (rs *relaySidecar) rawUpstreams() ([]string, error) {
	var out []string
	if rs.ic == nil {
		return nil, nil
	}
	for _, h := range rs.ic.held {
		spec := h.Spec()
		if spec.FormOf() != egress.FormTCPAuth {
			continue
		}
		if rs.b.Egress == nil {
			return nil, credentialFailed(fmt.Errorf("the tcp_auth credential %s names %s, but the kind declares no egress", h.ref, spec.Host))
		}
		allowed := egress.NewPolicy(rs.b.Egress.Hosts, tenantHosts(rs.b), nil)
		if ok, _, _ := allowed.Allow(spec.Host, egress.Port); !ok {
			return nil, credentialFailed(fmt.Errorf("the tcp_auth credential %s names %s, which is not in the kind's egress", h.ref, spec.Host))
		}
		out = append(out, net.JoinHostPort(spec.Host, strconv.Itoa(spec.Port)))
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

// startEgressProcess runs the lease's egress as a process of its own
// (ADR-051 decision 1): mesh lease-egress, in its own process group, as
// the node's own user — never a child of the node's process group, and
// never inside the pod. It drives the process over its control socket
// (decision 2): Start installs the policy and the sign rule the
// session's tokens read of the moment, Credentials hands it the
// lease's session token and DPoP key, and a goroutine streams its
// audit back into the node's existing reporters until close cancels it
// (decision 3: the actor owns the process's lifecycle, like a pod).
// rs.node.Tunnel and rs.laneHandler become forwarding handlers onto the
// process's own node and lane sockets: neither the sidecar nor a lane
// ever sees an address change.
func (rs *relaySidecar) startEgressProcess(ctx context.Context, policy *egress.Policy, b *meshapi.Brief, creds relaysidecar.Credentials, report, audit func(egress.Call)) error {
	dir, err := rs.r.leaseSocketDir(rs.b.LeaseID + "\x00egress")
	if err != nil {
		return err
	}
	ctlSock := filepath.Join(dir, egressControlSocket)
	nodeSock := filepath.Join(dir, egressNodeSocket)
	laneSock := filepath.Join(dir, egressLaneSocket)
	bin, err := relayBinary(rs.r.MeshBinary)
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	cmd := exec.Command(bin, "lease-egress", "--control", ctlSock, "--node", nodeSock, "--lane", laneSock)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return err
	}
	// cmd.Wait is called exactly once, here, for the process's whole
	// life: stopEgress and watchEgress both only ever read done.
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			rs.r.logf("nodev1: lease %s: lease-egress: %s", rs.b.LeaseID, sc.Text())
		}
	}()
	if err := waitListening(ctx, ctlSock, relayReadyWait); err != nil {
		stopEgressProcess(cmd, done)
		os.RemoveAll(dir)
		return fmt.Errorf("did not start: %w", err)
	}
	client := LeaseEgressClient{Socket: ctlSock}
	var specs []egress.RuleSpec
	if rs.ic != nil {
		specs = rs.ic.specs
	}
	started, err := client.Start(ctx, LeaseEgressStartRequest{Hosts: policy.Hosts(), Open: b.Egress == nil, Rules: specs})
	if err != nil {
		stopEgressProcess(cmd, done)
		os.RemoveAll(dir)
		return err
	}
	if !started.Started {
		stopEgressProcess(cmd, done)
		os.RemoveAll(dir)
		return fmt.Errorf("refused: %s", started.Refused)
	}
	replaced, err := client.Credentials(ctx, LeaseEgressCredentialsRequest{Key: creds.Key, Session: creds.Session})
	if err != nil {
		stopEgressProcess(cmd, done)
		os.RemoveAll(dir)
		return err
	}
	if !replaced.Replaced {
		stopEgressProcess(cmd, done)
		os.RemoveAll(dir)
		return fmt.Errorf("credentials refused: %s", replaced.Refused)
	}
	actx, cancel := context.WithCancel(context.Background())
	reqAudit := rs.r.interceptAudit(rs.b.LeaseID)
	go func() {
		for actx.Err() == nil {
			_ = client.Audit(actx, nil, func(ev AuditEvent) {
				switch ev.Kind {
				case "call":
					if ev.Call != nil && !ev.Call.Allowed {
						audit(*ev.Call)
					}
				case "done":
					if ev.Call != nil {
						report(*ev.Call)
					}
				case "request":
					if ev.Request != nil {
						reqAudit(*ev.Request)
					}
				}
			})
			if actx.Err() != nil {
				return
			}
			select {
			case <-actx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	rs.mu.Lock()
	rs.caPEM = started.CACertPEM
	rs.egressCmd, rs.egressDir, rs.egressAuditCancel, rs.egressDone = cmd, dir, cancel, done
	rs.mu.Unlock()
	rs.node.Tunnel = rs.forwardConnect(nodeSock, func(r *http.Request) bool { return egress.IsSpliceOnly(r.Context()) })
	rs.laneHandler = rs.forwardConnect(laneSock, nil)
	return nil
}

// forwardConnect is the node's CONNECT-forwarding handler onto the
// lease's egress process (ADR-051 decision 1): the node's own node and
// lane sockets forward every CONNECT here unchanged, so neither the
// sidecar nor a lane ever sees an address change. own, when it answers
// true for the request, re-sends the sidecar's own dial mark
// (relaysidecar.OwnHeader) on the forwarded CONNECT, since
// egress.SpliceOnly cannot cross the socket boundary — the process
// re-derives it there the same way relaysidecar.Node.ServeHTTP does
// for the in-process tunnel.
func (rs *relaySidecar) forwardConnect(sock string, own func(*http.Request) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mark := ""
		if own != nil && own(r) {
			mark = relaysidecar.OwnHeader + ": own\r\n"
		}
		var d net.Dialer
		up, err := d.DialContext(r.Context(), "unix", sock)
		if err != nil {
			egress.Refuse(w, "internal", "the lease's egress process: "+err.Error())
			return
		}
		if _, err := fmt.Fprintf(up, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", r.Host, r.Host, mark); err != nil {
			up.Close()
			egress.Refuse(w, "internal", "the lease's egress process: "+err.Error())
			return
		}
		br := bufio.NewReader(up)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			up.Close()
			egress.Refuse(w, "internal", "the lease's egress process: "+err.Error())
			return
		}
		if resp.StatusCode != http.StatusOK {
			for k, vv := range resp.Header {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			up.Close()
			return
		}
		resp.Body.Close()
		hj, ok := w.(http.Hijacker)
		if !ok {
			up.Close()
			egress.Refuse(w, "internal", "the relay cannot open a tunnel here")
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			up.Close()
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			conn.Close()
			up.Close()
			return
		}
		forwardPipe(conn, brw.Reader, up, br)
		conn.Close()
		up.Close()
	}
}

// forwardPipe copies the pod's (or lane's) hijacked connection, read
// through r (what the hijack buffered), to the lease's egress process
// and back, through upR (what the CONNECT handshake buffered there),
// until either side ends.
func forwardPipe(client net.Conn, r io.Reader, up net.Conn, upR io.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(up, r)
		closeWriteConn(up)
	}()
	go func() {
		defer wg.Done()
		io.Copy(client, upR)
		closeWriteConn(client)
	}()
	wg.Wait()
}

func closeWriteConn(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// stopEgressProcess ends the lease's egress process (ADR-051 decision
// 3: the actor owns its lifecycle, like a pod): SIGTERM, a bounded
// wait for its own exit, then SIGKILL — its own process group, never
// the node's own (ADR-051 decision 1), so a kill here never touches
// the node. done is the process's own Wait goroutine, started once
// when it was started: stopEgressProcess only ever reads it.
func stopEgressProcess(cmd *exec.Cmd, done chan struct{}) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
		return
	case <-time.After(egressStopWait):
	}
	_ = cmd.Process.Kill()
	<-done
}

// stopLeftoverEgress ends an egress process a node named in actor.json
// before it stopped (ADR-051 decision 5): there is no *exec.Cmd to
// signal or wait on across a restart, only the pid, so it signals the
// pid directly and polls for it to be gone rather than reading its
// exit through cmd.Wait. A pid already gone, or refusing the signal
// with ESRCH, is success, not an error.
func stopLeftoverEgress(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("signalling %d: %w", pid, err)
	}
	deadline := time.Now().Add(egressStopWait)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("killing %d: %w", pid, err)
	}
	return nil
}

// stopEgress ends the lease's egress process and removes its socket
// directory (ADR-051 decisions 1 and 3): the actor's own hold on it,
// released before the pod's so the process never outlives it, and
// close's own tail. Idempotent: a second call, or one before the
// process ever started, does nothing.
func (rs *relaySidecar) stopEgress() error {
	rs.mu.Lock()
	rs.egressStopping = true
	cmd, dir, auditCancel, done := rs.egressCmd, rs.egressDir, rs.egressAuditCancel, rs.egressDone
	rs.egressCmd, rs.egressDir, rs.egressAuditCancel, rs.egressDone = nil, "", nil, nil
	rs.mu.Unlock()
	if auditCancel != nil {
		auditCancel()
	}
	if cmd != nil && cmd.Process != nil {
		stopEgressProcess(cmd, done)
	}
	if dir != "" {
		os.RemoveAll(dir)
	}
	return nil
}

// watchEgress ends the lease, as a killed pod does, when the lease's
// egress process exits on its own (ADR-051 decision 3: the actor
// watches it as it watches the pod) — never when stopEgress stopped
// it, and never past ctx ending (the lease ending some other way).
func (rs *relaySidecar) watchEgress(ctx context.Context, cancel context.CancelCauseFunc) {
	rs.mu.Lock()
	done, cmd := rs.egressDone, rs.egressCmd
	rs.mu.Unlock()
	if done == nil || cmd == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-done:
	}
	rs.mu.Lock()
	stopping := rs.egressStopping
	rs.mu.Unlock()
	if stopping {
		return
	}
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	cancel(&killedError{code: code, why: fmt.Sprintf("%v: it exited %d", errEgressKilled, code)})
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
	minted, lanes := rs.minted, rs.lanes
	rs.mu.Unlock()
	if rs.node != nil {
		_ = rs.node.Close()
	}
	if lanes != nil {
		_ = lanes.Close()
	}
	if rs.own != nil {
		rs.own.Close()
		os.RemoveAll(rs.ownDir)
	}
	rs.stopEgress()
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

// relayLaneDir is the directory, in the lease's socket directory, of
// the socket its lanes reach its egress on; relayLaneSocket the socket.
const (
	relayLaneDir    = "l"
	relayLaneSocket = "s"
)

// openLaneTunnel serves the lease's egress to run_suite's lanes
// (lanes.go), which cannot reach the sidecar on its pod's loopback: a
// socket in a directory of its own, mounted into each lane, where each
// step's bridge pipes its HTTPS_PROXY. It answers CONNECT alone, on the
// lease's tunnel — its policy, its interception, its audit, as the
// session's CONNECT through the sidecar — and nothing else of the
// node's side: no renewal, no credentials, no node tools, no model.
// The lease's end closes it.
func (rs *relaySidecar) openLaneTunnel() (string, error) {
	var inner http.Handler
	switch {
	case rs.laneHandler != nil:
		inner = rs.laneHandler
	case rs.tunnel != nil:
		inner = rs.tunnel
	default:
		return "", errors.New("the lease has no egress tunnel")
	}
	dir := filepath.Join(rs.dir, relayLaneDir)
	if err := os.Mkdir(dir, 0o711); err != nil {
		return "", err
	}
	sock := filepath.Join(dir, relayLaneSocket)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return "", err
	}
	// a lane's user is the pod's, mapped as the session's is
	if err := os.Chmod(sock, 0o666); err != nil {
		ln.Close()
		return "", err
	}
	srv := &http.Server{ReadHeaderTimeout: 30 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "a lane reaches the lease's egress by CONNECT alone", http.StatusForbidden)
			return
		}
		// never the sidecar's own connection, whatever a step says
		r.Header.Del(relaysidecar.OwnHeader)
		inner.ServeHTTP(w, r)
	})}
	rs.mu.Lock()
	rs.lanes = srv
	rs.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			rs.r.logf("nodev1: lease %s: the lanes' tunnel: %v", rs.b.LeaseID, err)
		}
	}()
	return sock, nil
}
