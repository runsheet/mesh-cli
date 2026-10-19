// Package sessionproxy is the host-side MCP proxy a session reaches
// every MCP server through: streamable HTTP at /mcp/<server> on a unix
// socket, one upstream per server name. The proxy holds what the
// upstreams are reached on — a remote tool's secret headers, the
// runsheet bearer — so none of it is in the agent's env or on its disk:
// the agent names a server, the proxy adds the credential on the way out
// and masks it on the way back.
package sessionproxy

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
	"os"
	"strings"
	"sync"

	"projectmesh.io/mesh-cli/internal/mcprelay"
	"projectmesh.io/mesh-cli/internal/tools"
)

// Prefix is the path every server is served under: /mcp/<server>.
const Prefix = "/mcp/"

// forwarded are the only client headers sent upstream: what streamable
// HTTP needs. Anything else — Authorization, a cookie, a header an
// upstream's config names — never leaves the proxy.
var forwarded = []string{
	"Content-Type",
	"Accept",
	"Mcp-Session-Id",
	"Mcp-Protocol-Version",
	"Last-Event-Id",
}

// returned are the only upstream headers sent back to the client.
var returned = []string{
	"Content-Type",
	"Mcp-Session-Id",
	"Mcp-Protocol-Version",
}

// Upstream is one server the proxy fronts: Remote, Runsheet or Local.
type Upstream interface {
	endpoint() string
	// authorize sets the upstream's own credential on h; again is a
	// retry after a 401.
	authorize(ctx context.Context, h http.Header, again bool) ([]string, error)
	// retries401 says whether a 401 is asked again once.
	retries401() bool
}

// Remote is an HTTP MCP server reached on headers the proxy holds:
// Headers' values are secrets, set on every request and never taken
// from the client.
type Remote struct {
	URL     string
	Headers map[string]string
}

func (r Remote) endpoint() string { return r.URL }

func (r Remote) retries401() bool { return false }

func (r Remote) authorize(_ context.Context, h http.Header, _ bool) ([]string, error) {
	var secrets []string
	for k, v := range r.Headers {
		h.Set(k, v)
		secrets = append(secrets, secretParts(v)...)
	}
	return secrets, nil
}

// Runsheet is the gateway's /mcp, reached on a bearer Bearer gives per
// request: held in memory for that request alone, asked again after a
// 401 and tried once more, as mcprelay re-reads its token file.
type Runsheet struct {
	URL    string
	Bearer func(ctx context.Context) (string, error)
}

func (r Runsheet) endpoint() string { return r.URL }

func (r Runsheet) retries401() bool { return true }

func (r Runsheet) authorize(ctx context.Context, h http.Header, _ bool) ([]string, error) {
	if r.Bearer == nil {
		return nil, errors.New("no bearer")
	}
	tok, err := r.Bearer(ctx)
	if err != nil {
		return nil, fmt.Errorf("bearer: %w", err)
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return nil, errors.New("bearer is empty")
	}
	h.Set("Authorization", "Bearer "+tok)
	return []string{tok}, nil
}

// secretParts is a header value to mask and, for `<scheme> <credential>`,
// the credential alone, which an upstream may echo without its scheme.
func secretParts(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	out := []string{v}
	if _, cred, ok := strings.Cut(v, " "); ok && strings.TrimSpace(cred) != "" {
		out = append(out, strings.TrimSpace(cred))
	}
	return out
}

// maxSecrets bounds the masks remembered: a rotating bearer adds one
// every rotation.
const maxSecrets = 64

// Proxy serves each upstream at /mcp/<name>.
type Proxy struct {
	// HTTP is the client upstreams are reached on; a plain one with no
	// timeout when nil, since a tool call may stream for as long as it
	// runs.
	HTTP *http.Client
	// Logf takes the proxy's own lines, every one masked; nothing is
	// logged when nil.
	Logf func(format string, args ...any)

	mu        sync.Mutex
	upstreams map[string]Upstream
	secrets   []string
	children  map[string]*child
	// errs is each server's first error, as the proxy answered it.
	errs map[string]string

	srv  *http.Server
	ln   net.Listener
	path string
}

// New is a proxy fronting upstreams by name.
func New(upstreams map[string]Upstream) *Proxy {
	p := &Proxy{upstreams: map[string]Upstream{}}
	for name, u := range upstreams {
		p.Add(name, u)
	}
	return p
}

// Add serves u at /mcp/<name>, replacing any upstream of that name, and
// remembers a Remote's header values to mask.
func (p *Proxy) Add(name string, u Upstream) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.upstreams == nil {
		p.upstreams = map[string]Upstream{}
	}
	p.upstreams[name] = u
	switch r := u.(type) {
	case Remote:
		for _, v := range r.Headers {
			p.rememberLocked(secretParts(v))
		}
	case Local:
		for _, v := range r.Secrets {
			p.rememberLocked([]string{strings.TrimSpace(v)})
		}
	}
	if old, ok := p.children[name]; ok {
		delete(p.children, name)
		go old.close()
	}
}

func (p *Proxy) upstream(name string) (Upstream, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u, ok := p.upstreams[name]
	return u, ok
}

func (p *Proxy) remember(secrets []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rememberLocked(secrets)
}

func (p *Proxy) rememberLocked(secrets []string) {
	for _, s := range secrets {
		if s == "" || contains(p.secrets, s) {
			continue
		}
		p.secrets = append(p.secrets, s)
	}
	if n := len(p.secrets); n > maxSecrets {
		p.secrets = append([]string(nil), p.secrets[n-maxSecrets:]...)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// noteError keeps msg as name's first error, masked (FirstError).
func (p *Proxy) noteError(name, msg string) {
	msg = p.mask(msg)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.errs == nil {
		p.errs = map[string]string{}
	}
	if _, ok := p.errs[name]; !ok {
		p.errs[name] = msg
	}
}

// FirstError is the first error the proxy answered for server name —
// an upstream it could not reach, a bearer it could not get, a status
// that was not 2xx — in the words its JSON-RPC error carried
// ("sessionproxy: runsheet: …"), every secret masked; "" when none.
// The node names it in the session's tools note, since the agent's MCP
// client says only that the server failed.
func (p *Proxy) FirstError(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.errs[name]
}

// mask replaces every secret the proxy holds or has held in s.
func (p *Proxy) mask(s string) string {
	p.mu.Lock()
	env := make([]string, 0, len(p.secrets))
	for _, v := range p.secrets {
		env = append(env, "s="+v)
	}
	p.mu.Unlock()
	return tools.Redact(s, env)
}

func (p *Proxy) logf(format string, args ...any) {
	if p.Logf == nil {
		return
	}
	p.Logf("%s", p.mask(fmt.Sprintf(format, args...)))
}

// Listen serves the proxy on a unix socket at path, 0600, in the
// background until Close. A socket left at path by an earlier proxy is
// removed first. The socket's directory should be the session's own,
// unreadable to others, since the mode is set just after it is made.
func (p *Proxy) Listen(path string) error {
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
	srv := &http.Server{Handler: p}
	p.mu.Lock()
	p.srv, p.ln, p.path = srv, ln, path
	p.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.logf("sessionproxy: serve %s: %v", path, err)
		}
	}()
	return nil
}

// Close stops serving, removes the socket and kills every local server
// the proxy started, waiting for each to be reaped.
func (p *Proxy) Close() error {
	defer p.closeChildren()
	p.mu.Lock()
	srv, path := p.srv, p.path
	p.srv, p.ln, p.path = nil, nil, ""
	p.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Close()
	if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	return err
}

// ServeHTTP relays one request to the upstream its path names.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	name, ok := strings.CutPrefix(req.URL.Path, Prefix)
	name = strings.TrimSuffix(name, "/")
	u, found := p.upstream(name)
	if !ok || name == "" || strings.Contains(name, "/") || !found {
		writeError(w, http.StatusNotFound, -32601, fmt.Sprintf("no MCP server %q", name))
		return
	}
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(io.LimitReader(req.Body, mcprelay.MaxMessage+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, -32700, "reading the request: "+err.Error())
			return
		}
		if len(b) > mcprelay.MaxMessage {
			writeError(w, http.StatusRequestEntityTooLarge, -32600, "request too large")
			return
		}
		body = b
	}
	if l, ok := u.(Local); ok {
		p.serveLocal(w, req, name, l, body)
		return
	}
	resp, err := p.forward(req, u, body, false)
	if err == nil && resp.StatusCode == http.StatusUnauthorized && u.retries401() {
		resp.Body.Close()
		resp, err = p.forward(req, u, body, true)
	}
	if err != nil {
		msg := p.mask(err.Error())
		p.logf("sessionproxy: %s: %s", name, msg)
		p.noteError(name, fmt.Sprintf("sessionproxy: %s: %s", name, msg))
		writeError(w, http.StatusBadGateway, -32603, fmt.Sprintf("sessionproxy: %s: %s", name, msg))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		p.logf("sessionproxy: %s: %s %s answered %d", name, req.Method, u.endpoint(), resp.StatusCode)
		if req.Method == http.MethodPost {
			// a GET's 405 is how a server says it streams nothing
			p.noteError(name, fmt.Sprintf("sessionproxy: %s: %s %s answered %d", name, req.Method, u.endpoint(), resp.StatusCode))
		}
	}
	p.reply(w, resp)
}

// forward sends the request on to u with the client's MCP headers and
// u's own credential, nothing else of the client's.
func (p *Proxy) forward(req *http.Request, u Upstream, body []byte, again bool) (*http.Response, error) {
	out, err := http.NewRequestWithContext(req.Context(), req.Method, u.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, k := range forwarded {
		if v := req.Header.Values(k); len(v) > 0 {
			out.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	secrets, err := u.authorize(req.Context(), out.Header, again)
	p.remember(secrets)
	if err != nil {
		return nil, err
	}
	client := p.HTTP
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(out)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// reply relays the upstream's answer, masked: an event stream line by
// line as it arrives, anything else whole.
func (p *Proxy) reply(w http.ResponseWriter, resp *http.Response) {
	for _, k := range returned {
		if v := resp.Header.Values(k); len(v) > 0 {
			w.Header()[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, err := io.ReadAll(io.LimitReader(resp.Body, mcprelay.MaxMessage))
		if err != nil {
			p.logf("sessionproxy: reading the reply: %v", err)
		}
		masked := p.mask(string(b))
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.WriteString(w, masked)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	rd := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, err := rd.ReadString('\n')
		if line != "" {
			if _, werr := io.WriteString(w, p.mask(line)); werr != nil {
				return
			}
			if flusher != nil && strings.TrimRight(line, "\r\n") == "" {
				flusher.Flush()
			}
		}
		if err != nil {
			if flusher != nil {
				flusher.Flush()
			}
			if !errors.Is(err, io.EOF) {
				p.logf("sessionproxy: reading the stream: %v", err)
			}
			return
		}
	}
}

// writeError answers with a JSON-RPC error of the proxy's own.
func writeError(w http.ResponseWriter, status, code int, msg string) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error":   map[string]any{"code": code, "message": msg},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
