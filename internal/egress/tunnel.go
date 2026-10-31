// Package egress is a session's way out to the internet and the rule
// that decides it (projectmesh-io/api ADR 001 amendment, tool slots and
// egress; mesh-specs gaps 5 and 11). A Policy is the hosts a lease may
// reach: its kind's egress hosts, narrowed by its tenant's policy and
// never widened, plus any fixed upstreams its relay must reach itself.
// A Tunnel is the only exit: an HTTP CONNECT to host:443 that the
// Policy allows, whose TLS ClientHello names that same host, is dialled
// from the node and piped — or, for a host one of the lease's Rules
// names, terminated by the relay with its lease's CA (intercept.go);
// anything else — another port, plain-HTTP proxying, a host the Policy
// does not name, a server name that differs from the host asked — is
// refused, and each request is audited by host and port alone. A Tunnel is an http.Handler, so it serves on a relay's
// socket beside other routes, or on a unix socket of its own per lease
// (Listen), which a relay sidecar in the lease's pod can reach.
package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Port is the one port a tunnel reaches: HTTPS.
const Port = "443"

// Policy is the hosts a lease may reach by HTTPS, exactly (no wildcard,
// any case), on Port alone. The zero Policy allows nothing.
type Policy struct {
	hosts map[string]bool
	// fixed are the hosts that are fixed upstreams, not the kind's.
	fixed map[string]bool
	// ports are the other ports a fixed upstream named as host:port may
	// be reached on (a self-hosted Latchkey or vendor on a port of its
	// own); never a kind's host.
	ports map[string]map[string]bool
	// open allows any host on Port: a lease whose kind declares no
	// egress, whose session reached the internet by HTTPS before a
	// relay sidecar made the tunnel its only way out (Open).
	open bool
}

// Open is p allowing any host on Port as well, dialled on a public
// address alone as every host is: a lease whose kind declares no egress
// keeps the internet by HTTPS it had, now only through its tunnel.
func (p *Policy) Open() *Policy {
	cp := &Policy{hosts: map[string]bool{}, fixed: map[string]bool{}, ports: map[string]map[string]bool{}, open: true}
	if p != nil {
		for h := range p.hosts {
			cp.hosts[h] = true
		}
		for h := range p.fixed {
			cp.fixed[h] = true
		}
		for h, ports := range p.ports {
			cp.ports[h] = ports
		}
	}
	return cp
}

// NewPolicy is the lease's policy: kind, the kind's egress hosts, kept
// only where tenant — the tenant's own allowlist, nil when the tenant
// has none, which narrows nothing — names them too; then fixed, the
// upstreams the relay itself reaches (Latchkey, Mesh, the product's MCP
// host, the git and model vendors), which no narrowing removes. Hosts
// are compared lower-cased without a trailing dot.
func NewPolicy(kind []string, tenant []string, fixed []string) *Policy {
	p := &Policy{hosts: map[string]bool{}, fixed: map[string]bool{}}
	var allowed map[string]bool
	if tenant != nil {
		allowed = map[string]bool{}
		for _, h := range tenant {
			allowed[norm(h)] = true
		}
	}
	for _, h := range kind {
		if h = norm(h); h != "" && (allowed == nil || allowed[h]) {
			p.hosts[h] = true
		}
	}
	for _, h := range fixed {
		if host, port, err := net.SplitHostPort(strings.TrimSpace(h)); err == nil && port != Port {
			if host = norm(host); host != "" {
				if p.ports == nil {
					p.ports = map[string]map[string]bool{}
				}
				if p.ports[host] == nil {
					p.ports[host] = map[string]bool{}
				}
				p.ports[host][port] = true
			}
			continue
		} else if err == nil {
			h = host
		}
		if h = norm(h); h != "" {
			p.hosts[h] = true
			p.fixed[h] = true
		}
	}
	return p
}

// The rules a tunnel's request is allowed or refused by, as its Call
// names them: content-free names Mesh forwards to Tripline (projectmesh-io/api
// ADR 001 amendment, security events).
const (
	RuleHosts    = "egress.hosts"    // the kind's egress hosts
	RuleFixed    = "egress.fixed"    // a fixed upstream of the relay's own
	RuleOpen     = "egress.open"     // a kind with no egress: any host by HTTPS
	RulePort     = "egress.port"     // only port 443
	RuleHTTPS    = "egress.https"    // only HTTPS by CONNECT
	RuleSNI      = "egress.sni"      // the TLS server name is the host asked
	RuleHello    = "egress.hello"    // no TLS ClientHello came
	RuleDial     = "egress.dial"     // the host could not be reached on a public address
	RuleInternal = "egress.internal" // the relay could not open a tunnel
	RuleNone     = "egress.none"     // the session has no egress at all
)

// rule is the rule that allows host:port, which Allow has allowed.
func (p *Policy) rule(host, port string) string {
	h := norm(host)
	switch {
	case p == nil:
		return RuleHosts
	case port != Port && p.ports[h][port], p.fixed[h]:
		return RuleFixed
	case p.hosts[h]:
		return RuleHosts
	case p.open:
		return RuleOpen
	}
	return RuleHosts
}

func norm(h string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".") }

// Allow says whether a tunnel to host:port is allowed, and when not, a
// short code and why.
func (p *Policy) Allow(host, port string) (ok bool, code, why string) {
	switch {
	case p != nil && port != Port && p.ports[norm(host)][port]:
		return true, "", ""
	case port != Port:
		return false, "port", "only port 443 leaves the relay, not " + net.JoinHostPort(host, port)
	case p == nil || !p.open && !p.hosts[norm(host)]:
		return false, "host", host + " is not in the kind's egress"
	}
	return true, "", ""
}

// Hosts are the policy's hosts, for a log line.
func (p *Policy) Hosts() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.hosts))
	for h := range p.hosts {
		out = append(out, h)
	}
	return out
}

// Tunnel serves CONNECT host:443 for one lease under its Policy.
type Tunnel struct {
	Policy *Policy
	// Dial reaches an allowed host from the node; nil dials it on a
	// public address alone (publicDial), never one in a private,
	// loopback or link-local range a name may have been pointed at.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Audit hears each request the session made of the tunnel, allowed
	// or not: the host and port asked, whether it was allowed and why
	// not. It must not block.
	Audit func(Call)
	// Done hears each allowed tunnel once it has closed, with the bytes
	// it carried both ways. It must not block.
	Done func(Call)
	// Logf takes the tunnel's lines; nothing is logged when nil.
	Logf func(format string, args ...any)
	// HelloTimeout bounds the wait for the ClientHello after the tunnel
	// opens; 10 s when zero.
	HelloTimeout time.Duration

	// CA and Rules terminate TLS for the hosts a rule names, which the
	// Policy allows (intercept.go); with no CA, or no rule naming the
	// host, a tunnel is spliced by SNI and nothing past the ClientHello
	// is read.
	CA    *CA
	Rules []Rule
	// Request hears each request of a terminated tunnel once it is
	// answered: what the rules applied, never a value. It must not
	// block.
	Request func(Request)
	// Upstream carries a terminated tunnel's requests to their host:
	// nil is a transport of the tunnel's own, dialling with Dial and
	// verifying the host against the system roots (tests set one that
	// trusts their own server).
	Upstream http.RoundTripper

	mu        sync.Mutex
	srv       *http.Server
	path      string
	conns     map[net.Conn]bool
	transport *http.Transport
}

// Call is one request a session made of its tunnel: never a byte of
// what it sent.
type Call struct {
	Host    string
	Port    string
	Allowed bool
	// Refused is why it was refused, "" when it was allowed.
	Refused string
	// Rule names the rule that allowed or refused it (Rule*).
	Rule string
	// Bytes are what an allowed tunnel carried both ways, in Done.
	Bytes int64
}

// RefusedHeader names the relay's refusal of an egress request.
const RefusedHeader = "X-Mesh-Egress-Refused"

// IsEgress says req asks to reach somewhere else: a CONNECT, or a
// request in absolute form (a plain-HTTP proxy request).
func IsEgress(req *http.Request) bool {
	return req.Method == http.MethodConnect || (req.URL != nil && req.URL.IsAbs())
}

// Refuse answers an egress request no tunnel serves: 403, code in
// RefusedHeader.
func Refuse(w http.ResponseWriter, code, why string) {
	w.Header().Set(RefusedHeader, code)
	http.Error(w, "relay: egress refused: "+why, http.StatusForbidden)
}

// ServeHTTP answers a CONNECT the policy allows with a tunnel to the
// host, and refuses everything else.
func (t *Tunnel) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	host, port := req.Host, ""
	if h, pt, err := net.SplitHostPort(req.Host); err == nil {
		host, port = h, pt
	}
	call := Call{Host: host, Port: port}
	if req.Method != http.MethodConnect && req.URL != nil && req.URL.IsAbs() {
		call.Host, call.Port = req.URL.Hostname(), req.URL.Port()
	}
	refused := func(rule, why string) {
		call.Refused, call.Rule = why, rule
		t.audit(call)
		t.logf("egress: %s refused: %s", req.Host, why)
	}
	if req.Method != http.MethodConnect {
		refused(RuleHTTPS, "only HTTPS, by CONNECT to port 443, leaves the relay")
		Refuse(w, "not_https", call.Refused)
		return
	}
	if ok, code, why := t.Policy.Allow(host, port); !ok {
		rule := RuleHosts
		if code == "port" {
			rule = RulePort
		}
		refused(rule, why)
		Refuse(w, code, why)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		refused(RuleInternal, "the relay cannot open a tunnel here")
		Refuse(w, "internal", call.Refused)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		t.logf("egress: %s: hijack: %v", req.Host, err)
		return
	}
	defer conn.Close()
	t.track(conn, true)
	defer t.track(conn, false)
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	timeout := t.HelloTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	sni, hello, err := readClientHello(brw.Reader)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		refused(RuleHello, "no TLS ClientHello: "+err.Error())
		return
	}
	if norm(sni) != norm(host) {
		refused(RuleSNI, fmt.Sprintf("the TLS server name %q is not the host asked for", sni))
		return
	}
	if t.terminates(req.Context(), host) {
		// a host a rule names: the relay answers the session's TLS and
		// forwards each request on a connection of its own (intercept.go)
		call.Allowed, call.Rule = true, t.Policy.rule(host, port)
		t.audit(call)
		call.Bytes = t.terminate(conn, brw.Reader, hello, host, port)
		if t.Done != nil {
			t.Done(call)
		}
		return
	}
	dial := t.Dial
	if dial == nil {
		dial = publicDial
	}
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	up, err := dial(ctx, "tcp", net.JoinHostPort(host, port))
	cancel()
	if err != nil {
		refused(RuleDial, "dial: "+err.Error())
		return
	}
	defer up.Close()
	call.Allowed, call.Rule = true, t.Policy.rule(host, port)
	t.audit(call)
	n, err := up.Write(hello)
	if err == nil {
		sent, got := pipe(conn, brw.Reader, up)
		n += int(sent)
		call.Bytes = got
	}
	call.Bytes += int64(n)
	if t.Done != nil {
		t.Done(call)
	}
}

func (t *Tunnel) audit(c Call) {
	if t.Audit != nil {
		t.Audit(c)
	}
}

func (t *Tunnel) logf(format string, args ...any) {
	if t.Logf != nil {
		t.Logf(format, args...)
	}
}

// Listen serves the tunnel alone on a unix socket at path, 0600, until
// Close: a lease's own exit, for a relay that is not the node's
// sessionproxy (a sidecar in the lease's pod).
func (t *Tunnel) Listen(path string) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: t, ReadHeaderTimeout: 30 * time.Second}
	t.mu.Lock()
	t.srv, t.path = srv, path
	t.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.logf("egress: serve %s: %v", path, err)
		}
	}()
	return nil
}

// track keeps conn, a hijacked tunnel, for Close.
func (t *Tunnel) track(conn net.Conn, open bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !open {
		delete(t.conns, conn)
		return
	}
	if t.conns == nil {
		t.conns = map[net.Conn]bool{}
	}
	t.conns[conn] = true
}

// Close stops Listen's server, its tunnels with it, and removes the
// socket.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	srv, path := t.srv, t.path
	t.srv, t.path = nil, ""
	open := t.conns
	t.conns = nil
	t.mu.Unlock()
	for c := range open {
		c.Close()
	}
	t.mu.Lock()
	if t.transport != nil {
		t.transport.CloseIdleConnections()
	}
	t.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Close()
	if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	return err
}

// pipe copies client (read through r, which holds what the hijack
// buffered) to up and up to client until either side ends, and answers
// the bytes each way.
func pipe(client net.Conn, r io.Reader, up net.Conn) (sent, got int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sent, _ = io.Copy(up, r)
		closeWrite(up)
	}()
	go func() {
		defer wg.Done()
		got, _ = io.Copy(client, up)
		closeWrite(client)
	}()
	wg.Wait()
	return sent, got
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// errHelloRead stops the handshake once the ClientHello is read.
var errHelloRead = errors.New("client hello read")

// readClientHello reads a TLS ClientHello from r and answers its server
// name and every byte read, to send on to the host as they came.
func readClientHello(r *bufio.Reader) (string, []byte, error) {
	var seen bytes.Buffer
	var sni string
	got := false
	err := tls.Server(helloConn{r: io.TeeReader(r, &seen)}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			sni, got = h.ServerName, true
			return nil, errHelloRead
		},
	}).Handshake()
	if !got {
		if err == nil {
			err = errors.New("no ClientHello")
		}
		return "", nil, err
	}
	return sni, seen.Bytes(), nil
}

// helloConn is a net.Conn that only reads: the TLS server reading a
// ClientHello writes nothing the client sees.
type helloConn struct{ r io.Reader }

func (c helloConn) Read(b []byte) (int, error)     { return c.r.Read(b) }
func (helloConn) Write(b []byte) (int, error)      { return len(b), nil }
func (helloConn) Close() error                     { return nil }
func (helloConn) LocalAddr() net.Addr              { return nil }
func (helloConn) RemoteAddr() net.Addr             { return nil }
func (helloConn) SetDeadline(time.Time) error      { return nil }
func (helloConn) SetReadDeadline(time.Time) error  { return nil }
func (helloConn) SetWriteDeadline(time.Time) error { return nil }

// publicDial dials addr on one of its host's public addresses: a name
// that resolves only into a private, loopback, link-local, CGNAT or
// unspecified range is refused, so an allowed name cannot be pointed at
// the node's LAN or host.
func publicDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	var last error = fmt.Errorf("%s resolves to no public address", host)
	for _, ip := range ips {
		if !publicIP(ip.IP) {
			continue
		}
		c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

// cgnat is 100.64.0.0/10, which a tailnet uses.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// publicIP says ip is a global unicast address in no private range.
func publicIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !cgnat.Contains(ip)
}
