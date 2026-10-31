package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TLS termination for the hosts a lease's rules name (mesh-specs gaps
// 31, 28 and 32). A tunnel splices every host by SNI and reads nothing
// past the ClientHello — except a host one of its Rules Terminates,
// when it has a CA: there the tunnel answers the session's TLS itself
// with a certificate the lease's CA mints for the host (ca.go), reads
// each HTTP/1.1 request, lets every rule for the host Apply to it (add
// or replace a header, or refuse it), and forwards it to the host on a
// connection of the node's own, verified against the system roots.
// Bodies stream both ways as they come: nothing is buffered, every
// write flushed. Each request is told to Request with what the rules
// applied, never a header's value.
//
// A rule is the seam later gaps plug into: gap 31's SignRule (the
// lease's session token on its registries' prefixes) is the first; gap
// 28's injection rules (a header, bearer, basic, query or template from
// Latchkey) and gap 32's federation exchange are each a Rule too, the
// CA made over every rule's hosts.

// Rule acts on the requests of the hosts it terminates.
type Rule interface {
	// Terminates says the tunnel terminates TLS for host (lower-cased,
	// no port or trailing dot) so that Apply sees its requests. A host
	// no rule terminates is spliced untouched.
	Terminates(host string) bool
	// Apply sees one request to a host it terminates, before it goes
	// upstream: req.URL is absolute (https://host[:port], the path and
	// query as the session sent them) and req.Host the same authority.
	// It may change req's headers or query; it answers what it applied,
	// nil when the request is none of its business, and an error that
	// refuses the request (with what it would have applied, for the
	// audit): a *Refusal says how, any other is a 502 rule_failed. It
	// must not read the body.
	Apply(req *http.Request) (*Applied, error)
}

// Applied is what a rule did to a request, for the audit: names only,
// never a value.
type Applied struct {
	// Rule names the rule (egress.registries, ...).
	Rule string
	// Ref is what it applied: a registry prefix, a secret's ref.
	Ref string
	// Form is how: dpop, bearer, header, ...
	Form string
	// Kind is what Ref names (secret, connection), when a rule says.
	Kind string
	// Version is the version of what it applied, when it has one.
	Version string
}

// Refusal is a rule's refusal of a request: the status the session is
// answered and the code in ErrorHeader.
type Refusal struct {
	Status  int
	Code    string
	Message string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// ErrorHeader names why the relay answered a terminated request itself.
const ErrorHeader = "X-Mesh-Error"

// Request is one request of a terminated tunnel, once answered: never
// its path, a header or a byte of its body.
type Request struct {
	Host   string
	Port   string
	Method string
	// Applied is what each rule applied; empty when none did.
	Applied []Applied
	// Status is the answer's status, upstream's or the relay's own.
	Status int
	// Refused is the relay's code when it answered itself (ErrorHeader).
	Refused string
	// Bytes are the request's and answer's bodies, both ways.
	Bytes int64
}

type spliceKey struct{}

// SpliceOnly marks ctx's CONNECT as one the tunnel splices whatever its
// rules: a relay sidecar's own upstream connection (its MCP, git and
// registry clients speak TLS to the host themselves, on their own
// credentials), never the session's.
func SpliceOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, spliceKey{}, true)
}

// IsSpliceOnly says ctx's CONNECT was marked SpliceOnly.
func IsSpliceOnly(ctx context.Context) bool { return spliceOnly(ctx) }

func spliceOnly(ctx context.Context) bool {
	v, _ := ctx.Value(spliceKey{}).(bool)
	return v
}

// terminates says the tunnel terminates host for this CONNECT.
func (t *Tunnel) terminates(ctx context.Context, host string) bool {
	if t.CA == nil || spliceOnly(ctx) {
		return false
	}
	h := norm(host)
	for _, r := range t.Rules {
		if r.Terminates(h) {
			return true
		}
	}
	return false
}

// rulesFor are the rules that terminate host.
func (t *Tunnel) rulesFor(host string) []Rule {
	var out []Rule
	h := norm(host)
	for _, r := range t.Rules {
		if r.Terminates(h) {
			out = append(out, r)
		}
	}
	return out
}

// terminate serves the session's TLS on conn to host:port: the
// ClientHello already read (hello), the rest on r. It returns once the
// session closes the connection; the bytes it carried both ways, as
// they crossed the wire, are its answer.
func (t *Tunnel) terminate(conn net.Conn, r io.Reader, hello []byte, host, port string) int64 {
	raw := &countedConn{Conn: conn, r: io.MultiReader(bytes.NewReader(hello), r)}
	tc := tls.Server(raw, &tls.Config{
		MinVersion: tls.VersionTLS12,
		// HTTP/1.1 to the session: keep-alive, streaming both ways
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return t.CA.Leaf(host)
		},
	})
	ln := &oneConn{c: tc, done: make(chan struct{})}
	srv := &http.Server{
		Handler:           t.interceptor(host, port),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(logWriter{t: t, host: host}, "", 0),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateClosed || s == http.StateHijacked {
				ln.Close()
			}
		},
	}
	_ = srv.Serve(ln)
	<-ln.done
	return raw.n.Load()
}

// interceptor is the handler of a terminated tunnel to host:port.
func (t *Tunnel) interceptor(host, port string) http.Handler {
	rules := t.rulesFor(host)
	authority := host
	if port != Port {
		authority = net.JoinHostPort(host, port)
	}
	transport := t.upstream()
	errLog := log.New(logWriter{t: t, host: host}, "", 0)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		cw := &countedWriter{ResponseWriter: w}
		rec := Request{Host: host, Port: port, Method: req.Method}
		defer func() {
			rec.Status = cw.status
			if rec.Status == 0 {
				rec.Status = http.StatusOK
			}
			rec.Bytes = cw.n
			if req.Body != nil {
				if b, ok := req.Body.(*countedBody); ok {
					rec.Bytes += b.n.Load()
				}
			}
			if t.Request != nil {
				t.Request(rec)
			}
		}()
		refuse := func(status int, code, msg string) {
			rec.Refused = code
			cw.Header().Set(ErrorHeader, code)
			http.Error(cw, "relay: "+msg, status)
		}
		// the request is for the tunnel's host and no other: a Host or
		// absolute URL naming another would be signed for the one and
		// sent to the other
		if !sameHost(req.Host, host) || (req.URL.Host != "" && !sameHost(req.URL.Host, host)) {
			refuse(http.StatusMisdirectedRequest, "misdirected", "this connection is to "+host+" alone")
			return
		}
		req.URL.Scheme, req.URL.Host, req.Host = "https", authority, authority
		for _, r := range rules {
			ap, err := r.Apply(req)
			if ap != nil {
				rec.Applied = append(rec.Applied, *ap)
			}
			if err != nil {
				var ref *Refusal
				if errors.As(err, &ref) {
					refuse(ref.Status, ref.Code, ref.Message)
				} else {
					refuse(http.StatusBadGateway, "rule_failed", err.Error())
				}
				t.logf("egress: %s %s refused: %v", req.Method, host, err)
				return
			}
		}
		if req.Body != nil && req.Body != http.NoBody {
			req.Body = &countedBody{ReadCloser: req.Body}
		}
		rp := &httputil.ReverseProxy{
			// the request is already absolute on the tunnel's host;
			// Rewrite mode adds no X-Forwarded-* of its own
			Rewrite:   func(*httputil.ProxyRequest) {},
			Transport: transport,
			// every write reaches the session as it comes
			FlushInterval: -1,
			ErrorLog:      errLog,
			ErrorHandler: func(_ http.ResponseWriter, _ *http.Request, err error) {
				t.logf("egress: %s %s: upstream: %v", req.Method, host, err)
				if cw.status != 0 {
					// the answer had begun: it ends cut short
					return
				}
				refuse(http.StatusBadGateway, "upstream_unreachable", "the host did not answer")
			},
		}
		rp.ServeHTTP(cw, req)
	})
}

// upstream is what carries a terminated tunnel's requests to their
// host: Upstream, or a transport of the tunnel's own, dialling with Dial
// (a public address alone when nil), verifying against the system
// roots, using no proxy and leaving the session's encodings as they
// are.
func (t *Tunnel) upstream() http.RoundTripper {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Upstream != nil {
		return t.Upstream
	}
	if t.transport == nil {
		dial := t.Dial
		if dial == nil {
			dial = publicDial
		}
		t.transport = &http.Transport{
			DialContext:           dial,
			ForceAttemptHTTP2:     true,
			DisableCompression:    true,
			MaxIdleConns:          16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   30 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	return t.transport
}

// sameHost says authority (host or host:port) names host.
func sameHost(authority, host string) bool {
	h := authority
	if hh, _, err := net.SplitHostPort(authority); err == nil {
		h = hh
	}
	return norm(h) == norm(host)
}

// countedConn is the tunnel's raw connection: what the hijack and the
// ClientHello read already, then the rest; the bytes both ways counted.
type countedConn struct {
	net.Conn
	r io.Reader
	n atomic.Int64
}

func (c *countedConn) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// countedBody counts a request body as it is read.
type countedBody struct {
	io.ReadCloser
	n atomic.Int64
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

// countedWriter counts an answer's body and keeps its status; Unwrap
// lets the reverse proxy flush (and hijack, for an upgrade) through it.
type countedWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *countedWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countedWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

func (w *countedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// oneConn is a listener of one connection: served once, then Accept
// waits until Close, which the server's ConnState calls once the
// connection is done.
type oneConn struct {
	mu   sync.Mutex
	c    net.Conn
	done chan struct{}
	once sync.Once
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.c
	l.c = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConn) Addr() net.Addr { return oneAddr{} }

type oneAddr struct{}

func (oneAddr) Network() string { return "tunnel" }
func (oneAddr) String() string  { return "tunnel" }

// logWriter puts the server's and reverse proxy's own lines (a session
// refusing the relay's certificate, an upstream gone mid-answer) in the
// tunnel's log.
type logWriter struct {
	t    *Tunnel
	host string
}

func (w logWriter) Write(p []byte) (int, error) {
	w.t.logf("egress: %s: %s", w.host, strings.TrimSpace(string(p)))
	return len(p), nil
}
