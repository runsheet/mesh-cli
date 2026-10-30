package relaysidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/registryrelay"
	"projectmesh.io/mesh-cli/internal/secreport"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

// DefaultNodeToolsServer is the node tools' server name, as nodev1
// serves them (nodev1.NodeToolsServer).
const DefaultNodeToolsServer = "mesh-node"

// tokenRefreshBefore is how long before its end a held token is renewed.
const tokenRefreshBefore = time.Minute

// gitRefreshBefore is how long before its end the git token is renewed:
// room for a long fetch to start, as the node's own grant leaves.
const gitRefreshBefore = 2 * time.Minute

// Sidecar is one lease's relay in its pod.
type Sidecar struct {
	// NodeTools is the node tools' server name; DefaultNodeToolsServer
	// when empty.
	NodeTools string
	// Logf takes the sidecar's lines; never a credential.
	Logf func(format string, args ...any)

	creds    Credentials
	key      *dpop.Key
	node     *http.Client // plain HTTP to the node on the lease's socket
	toNode   *httputil.ReverseProxy
	upstream *http.Client // every upstream, through the node's tunnel
	dialer   *Dialer
	proxy    *sessionproxy.Proxy
	relay    *gitrelay.Relay
	registry *registryrelay.Relay
	access   chan meshapi.AccessRecord
	security chan meshapi.SecurityReport
	done     chan struct{}

	mu      sync.Mutex
	session Token
	git     Token
	addr    string // the loopback address served, once Serve runs
	refused []refusal
	srvs    []*http.Server
	closed  bool
}

// refusal is the relay's refusal of a push, kept so the push tool's
// answer names it in the relay's own words.
type refusal struct {
	at     time.Time
	reason string
}

// New is the sidecar for creds, reaching the node on nodeSocket.
func New(creds Credentials, nodeSocket string) (*Sidecar, error) {
	if err := creds.Validate(); err != nil {
		return nil, err
	}
	key, err := dpop.KeyFromHandoff(creds.Key)
	if err != nil {
		return nil, err
	}
	s := &Sidecar{creds: creds, key: key, session: creds.Session, dialer: &Dialer{Socket: nodeSocket},
		access: make(chan meshapi.AccessRecord, 1024), security: make(chan meshapi.SecurityReport, 256), done: make(chan struct{})}
	if creds.Git != nil {
		s.git = creds.Git.Token
	}
	s.node = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", nodeSocket)
		},
	}}
	s.toNode = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "node", "node"
		},
		Transport:     s.node.Transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.logf("relaysidecar: forwarding %s to the node: %v", r.URL.Path, err)
			http.Error(w, "relay: the node did not answer", http.StatusBadGateway)
		},
	}
	s.upstream = &http.Client{Transport: s.dialer.Transport()}
	ups := map[string]sessionproxy.Upstream{}
	for _, srv := range creds.Servers {
		ups[srv.Name] = sessionproxy.Product{URL: srv.URL, Bearer: s.sessionToken, Key: key}
	}
	s.proxy = sessionproxy.New(ups)
	s.proxy.HTTP = s.upstream
	s.proxy.Logf = s.logf
	// an egress request the sidecar refuses itself (plain HTTP, which
	// never reaches the node's tunnel) is passed to the node to report
	s.proxy.EgressRefused = s.egressRefused
	if creds.Tools != nil {
		s.proxy.Tools = sessionproxy.ToolFilter(creds.Tools)
	}
	if len(creds.Watch) > 0 {
		s.proxy.Observe = s.observe
	}
	if len(creds.API) > 0 {
		s.proxy.Latchkey = &sessionproxy.Latchkey{
			URL: creds.Latchkey, Token: s.sessionToken, Key: key,
			Allow: APIAllows(creds.API),
			Audit: func(c sessionproxy.APICall) {
				s.record(meshapi.AccessRecord{Kind: "api", Vendor: c.Vendor, Method: c.Method, Path: c.Path, Outcome: c.Outcome, Reason: c.Reason, Bytes: c.Bytes, At: c.At.UTC()})
			},
		}
	}
	if creds.Registry != "" {
		// the lease's registry, reached through the node's tunnel on the
		// lease's session token
		s.registry = &registryrelay.Relay{Upstream: creds.Registry, HTTP: s.upstream, Token: s.sessionToken, Key: key, Logf: s.logf}
		s.proxy.Registry = s.registry
	}
	if g := creds.Git; g != nil {
		vendor, err := gitrelay.VendorNamed(g.Policy.Vendor)
		if err != nil {
			return nil, err
		}
		upstream := g.Upstream
		if upstream == "" {
			upstream = strings.TrimRight(g.Policy.Base, "/")
		}
		s.relay = &gitrelay.Relay{
			Policy: g.Policy, Upstream: upstream, Vendor: vendor, HTTP: s.upstream,
			RefreshBefore: gitRefreshBefore,
			Mint:          s.gitToken,
			IsAncestor:    checkoutAncestry(g.Checkout),
			Mirror:        g.Mirror,
			Audit:         s.gitEvent,
		}
		s.proxy.Git = s.relay
	}
	go s.postAccess()
	go s.postSecurity()
	return s, nil
}

func (s *Sidecar) nodeTools() string {
	if s.NodeTools != "" {
		return s.NodeTools
	}
	return DefaultNodeToolsServer
}

func (s *Sidecar) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// ServeHTTP serves the session's routes on the pod's loopback: CONNECT
// through the node's tunnel, the node tools and the Stop hook and the
// model forwarded to the node unchanged, and the rest (/git/, /mcp/
// <product server>, /latchkey/<vendor>/) on the lease's credentials.
func (s *Sidecar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		s.serveConnect(w, r)
	case r.URL.Path == StopPath,
		strings.HasPrefix(r.URL.Path, sessionproxy.ModelPrefix),
		strings.TrimSuffix(r.URL.Path, "/") == sessionproxy.Prefix+s.nodeTools():
		s.toNode.ServeHTTP(w, r)
	case strings.HasPrefix(r.URL.Path, "/relay/"):
		// the node's routes and the push control are never the
		// session's to call
		http.NotFound(w, r)
	default:
		s.proxy.ServeHTTP(w, r)
	}
}

// serveConnect is the session's egress proxy (HTTPS_PROXY): a CONNECT
// the node's tunnel allows is piped, one it refuses is answered 403 with
// the node's reason.
func (s *Sidecar) serveConnect(w http.ResponseWriter, r *http.Request) {
	addr := r.Host
	if addr == "" {
		addr = r.URL.Host
	}
	up, err := s.dialer.DialContext(r.Context(), "tcp", addr)
	if err != nil {
		var ref *RefusedError
		if errors.As(err, &ref) {
			http.Error(w, "relay: "+ref.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, "relay: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "relay: cannot tunnel", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := io.WriteString(down, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		down.Close()
		up.Close()
		return
	}
	if n := buf.Reader.Buffered(); n > 0 {
		b, _ := buf.Reader.Peek(n)
		if _, err := up.Write(b); err != nil {
			down.Close()
			up.Close()
			return
		}
	}
	pipe(down, up)
}

// pipe copies both ways until either side ends.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}

// sessionToken is the lease's session token, renewed through the node
// near its end.
func (s *Sidecar) sessionToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	tok := s.session
	s.mu.Unlock()
	if tok.Value != "" && (tok.ExpiresAt.IsZero() || time.Now().Add(tokenRefreshBefore).Before(tok.ExpiresAt)) {
		return tok.Value, nil
	}
	ren, err := s.renew(ctx, RenewRequest{})
	if err != nil {
		return "", err
	}
	return ren.Session.Value, nil
}

// gitToken is the relay's Mint: the held git token while it lives, else
// one the node mints again.
func (s *Sidecar) gitToken(ctx context.Context) (gitrelay.Token, error) {
	s.mu.Lock()
	tok := s.git
	s.mu.Unlock()
	if tok.Value != "" && (tok.ExpiresAt.IsZero() || time.Now().Add(gitRefreshBefore).Before(tok.ExpiresAt)) {
		return gitrelay.Token{Value: tok.Value, ExpiresAt: tok.ExpiresAt}, nil
	}
	ren, err := s.renew(ctx, RenewRequest{Git: true})
	if err != nil {
		return gitrelay.Token{}, err
	}
	if ren.Git == nil || ren.Git.Value == "" {
		return gitrelay.Token{}, errors.New("the node renewed no git token")
	}
	return gitrelay.Token{Value: ren.Git.Value, ExpiresAt: ren.Git.ExpiresAt}, nil
}

// renew asks the node for the lease's tokens again and holds them.
func (s *Sidecar) renew(ctx context.Context, req RenewRequest) (Renewal, error) {
	b, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node"+RenewPath, bytes.NewReader(b))
	if err != nil {
		return Renewal{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := s.node.Do(hr)
	if err != nil {
		return Renewal{}, fmt.Errorf("relaysidecar: renewing through the node: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Renewal{}, fmt.Errorf("relaysidecar: the node refused the renewal: %d", resp.StatusCode)
	}
	var out Renewal
	if err := json.NewDecoder(io.LimitReader(resp.Body, renewBodyMax)).Decode(&out); err != nil {
		return Renewal{}, fmt.Errorf("relaysidecar: the node's renewal: %w", err)
	}
	if out.Session.Value == "" {
		return Renewal{}, errors.New("relaysidecar: the node renewed no session token")
	}
	s.mu.Lock()
	s.session = out.Session
	if out.Git != nil && out.Git.Value != "" {
		s.git = *out.Git
	}
	s.mu.Unlock()
	return out, nil
}

// gitEvent is the relay's audit: an AccessRecord for the node, and a
// refused push kept for the push tool's answer.
func (s *Sidecar) gitEvent(e gitrelay.Event) {
	rec := meshapi.AccessRecord{Kind: "git", Vendor: e.Vendor, Repo: e.Repo, Service: e.Service, Refs: e.Refs, Outcome: e.Outcome, Reason: e.Reason, Bytes: e.Bytes, At: e.At.UTC()}
	for _, p := range e.Pushed {
		rec.Pushed = append(rec.Pushed, meshapi.PushedRef{Ref: p.Ref, SHA: p.SHA})
	}
	if e.Service == "receive-pack" && e.Outcome == "refused" {
		s.mu.Lock()
		s.refused = append(s.refused, refusal{at: time.Now(), reason: e.Reason})
		if len(s.refused) > 32 {
			s.refused = s.refused[len(s.refused)-32:]
		}
		s.mu.Unlock()
	}
	s.record(rec)
}

// refusalSince is the relay's latest push refusal since t, "" for none.
func (s *Sidecar) refusalSince(t time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.refused) - 1; i >= 0; i-- {
		if s.refused[i].at.Before(t) {
			break
		}
		return s.refused[i].reason
	}
	return ""
}

// record queues rec for the node; a full queue drops it, logged.
func (s *Sidecar) record(rec meshapi.AccessRecord) {
	select {
	case s.access <- rec:
	default:
		s.logf("relaysidecar: an AccessRecord was dropped: the node is not taking them")
	}
}

// postAccess sends the queued AccessRecords to the node in batches.
func (s *Sidecar) postAccess() {
	for {
		var batch []meshapi.AccessRecord
		select {
		case rec := <-s.access:
			batch = append(batch, rec)
		case <-s.done:
			return
		}
	drain:
		for len(batch) < accessMax {
			select {
			case rec := <-s.access:
				batch = append(batch, rec)
			default:
				break drain
			}
		}
		s.sendAccess(batch)
	}
}

// egressRefused queues the report of an egress request the sidecar
// refused itself, by host and rule alone.
func (s *Sidecar) egressRefused(c egress.Call) {
	rep, ok := secreport.Egress(c, time.Now())
	if !ok {
		return
	}
	select {
	case s.security <- rep:
	default:
		s.logf("relaysidecar: a security report was dropped: the node is not taking them")
	}
}

// postSecurity sends the queued security reports to the node in
// batches, as postAccess does AccessRecords.
func (s *Sidecar) postSecurity() {
	for {
		var batch []meshapi.SecurityReport
		select {
		case rep := <-s.security:
			batch = append(batch, rep)
		case <-s.done:
			return
		}
	drain:
		for len(batch) < accessMax {
			select {
			case rep := <-s.security:
				batch = append(batch, rep)
			default:
				break drain
			}
		}
		s.send(SecurityPath, batch, len(batch), "security reports")
	}
}

// send posts v, n records of what, to the node at path.
func (s *Sidecar) send(path string, v any, n int, what string) {
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node"+path, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.node.Do(req)
	if err != nil {
		s.logf("relaysidecar: %d %s did not reach the node: %v", n, what, err)
		return
	}
	resp.Body.Close()
}

// Flush sends every queued AccessRecord and security report now.
func (s *Sidecar) Flush() {
	var reps []meshapi.SecurityReport
	for {
		select {
		case rep := <-s.security:
			reps = append(reps, rep)
			continue
		default:
		}
		break
	}
	if len(reps) > 0 {
		s.send(SecurityPath, reps, len(reps), "security reports")
	}
	var batch []meshapi.AccessRecord
	for {
		select {
		case rec := <-s.access:
			batch = append(batch, rec)
			continue
		default:
		}
		break
	}
	if len(batch) > 0 {
		s.sendAccess(batch)
	}
}

func (s *Sidecar) sendAccess(batch []meshapi.AccessRecord) {
	s.send(AccessPath, batch, len(batch), "AccessRecords")
}

// Serve serves the session's routes on ln (the pod's loopback) until
// Close.
func (s *Sidecar) Serve(ln net.Listener) error {
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return http.ErrServerClosed
	}
	s.addr = ln.Addr().String()
	s.srvs = append(s.srvs, srv)
	s.mu.Unlock()
	return srv.Serve(ln)
}

// ServeControl serves the node's push control on a unix socket at path
// until Close: the push tool's one way to push, under the same rules as
// the session's own git push.
func (s *Sidecar) ServeControl(path string) error {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	// the node's own UID connects: the directory is the gate, mounted
	// into this container alone
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PushPath, s.servePush)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return http.ErrServerClosed
	}
	s.srvs = append(s.srvs, srv)
	s.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logf("relaysidecar: the control socket: %v", err)
		}
	}()
	return nil
}

// Close stops serving and sends what AccessRecords remain.
func (s *Sidecar) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	srvs := s.srvs
	s.mu.Unlock()
	for _, srv := range srvs {
		_ = srv.Close()
	}
	close(s.done)
	s.Flush()
	return nil
}

// base is the loopback root the sidecar serves, once Serve runs.
func (s *Sidecar) base() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.addr == "" {
		return "", errors.New("relaysidecar: not serving yet")
	}
	return (&url.URL{Scheme: "http", Host: s.addr}).String(), nil
}

// observe sends the node a watched product tool's call and its answer.
// It must not block the proxy, so it sends in the background.
func (s *Sidecar) observe(server string, request, response []byte) {
	var call struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(request, &call) != nil || call.Method != "tools/call" || !s.watched(call.Params.Name) {
		return
	}
	b, err := json.Marshal(Observed{Server: server, Request: request, Response: response})
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node"+ObservePath, bytes.NewReader(b))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.node.Do(req)
		if err != nil {
			s.logf("relaysidecar: a call of %s did not reach the node: %v", call.Params.Name, err)
			return
		}
		resp.Body.Close()
	}()
}

// watched says name is one of the tools the node watches.
func (s *Sidecar) watched(name string) bool {
	for _, w := range s.creds.Watch {
		if name == w {
			return true
		}
	}
	return false
}
