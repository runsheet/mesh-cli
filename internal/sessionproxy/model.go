package sessionproxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"projectmesh.io/mesh-cli/internal/egress"
)

// No model key in the session (projectmesh-io/api ADR 001 amendment
// "model relay", 2026-10-30): the session's harness reaches its model at
// /model/<provider>/<path> on the lease's relay — ANTHROPIC_BASE_URL for
// Claude Code, OPENAI_BASE_URL for Codex — holding only a dummy
// credential. The proxy strips whatever authorisation the session sent,
// sets the lease's real credential, which only the node holds, and
// streams the answer back as it arrives. An expired credential, or an
// upstream's 401, has the credential read again once, and the request
// asked again, since nothing has gone back to the session yet.

// ModelPrefix is the path a session reaches its model under:
// /model/<provider>/<path>.
const ModelPrefix = "/model/"

// DefaultModelUpstreams are each provider's API origin.
var DefaultModelUpstreams = map[string]string{
	"anthropic": "https://api.anthropic.com",
	"openai":    "https://api.openai.com",
}

// modelBodyMax caps a model request's body, which is held so the request
// can be asked again after a refresh: the Messages API's own limit, with
// room.
const modelBodyMax = 40 << 20

// modelDropped are the client headers never sent upstream: the session's
// own (dummy) credential, cookies, and the hop-by-hop headers a proxy
// must not forward.
var modelDropped = map[string]bool{
	"Authorization": true, "X-Api-Key": true, "Cookie": true, "Proxy-Authorization": true,
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Host": true, "Content-Length": true,
}

// modelHeld are the upstream headers never sent back: hop-by-hop, a
// length the stream no longer has, and cookies.
var modelHeld = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Content-Length": true, "Set-Cookie": true,
}

// ErrModelRefused, wrapped by a Credential's error, refuses the request
// as the relay's own 403 — a provider the lease's account is not for —
// rather than a 502 the credential could not be read.
var ErrModelRefused = errors.New("refused")

// ModelCredential is how one request carries the lease's credential:
// Header set to Value (Authorization: Bearer <token>, or X-Api-Key:
// <key>).
type ModelCredential struct {
	Header string
	Value  string
}

// Model is the lease's model route.
type Model struct {
	// Upstreams are each provider's origin; DefaultModelUpstreams when
	// nil. A provider not named is refused.
	Upstreams map[string]string
	// Credential answers the provider's credential, held for this one
	// request; fresh reads it again (the upstream refused the last one).
	// An error refuses the request: the provider is not the lease's, or
	// the credential cannot be read.
	Credential func(ctx context.Context, provider string, fresh bool) (ModelCredential, error)
	// Log hears every call once it ends; it must not block.
	Log func(ModelCall)
	// Paths, when set, are the only paths a request may name, per
	// provider: the node's model forwarder for a lease's relay sidecar
	// (ForwarderPaths), the one place the node still reads HTTP a pod
	// sent it. nil allows any path, as a host session's relay does.
	Paths map[string][]string
	// Transport sends each request; http.DefaultTransport when nil. A
	// proxy follows no redirect, so it is a RoundTripper.
	Transport http.RoundTripper
	// Inspect, when set, sees each request's body, held whole: an error
	// refuses the request 403 credential_exposed, never sent (the lease's
	// canaries, mesh-specs gap 28 item 5).
	Inspect func(body []byte) error
}

// ModelCall is one model request a session made through the relay:
// never a body, a query or a credential.
type ModelCall struct {
	Provider string
	Method   string
	Path     string
	// Status is what the upstream answered; 0 when the relay refused or
	// could not reach it.
	Status int
	// Bytes is the answer's length as streamed back.
	Bytes int64
	// Latency is from the request to the answer's end.
	Latency time.Duration
	// Refreshed is whether the credential was read again and the request
	// asked again.
	Refreshed bool
	// Err says why the relay refused or failed it.
	Err string
	// ErrorType and ErrorMessage are the vendor's own error, read from an
	// answer of 400 or more (Anthropic's {"type":"error","error":{"type",
	// "message"}}, OpenAI's {"error":{"type"|"code","message"}}): its
	// type and message alone, never the rest of the body, the message
	// bounded to MaxVendorMessage and redacted of the lease's values
	// (mesh-cli ADR-048). Blank when the answer was not one of those.
	ErrorType    string
	ErrorMessage string
}

// MaxVendorMessage bounds a vendor error's message as a call keeps it.
const MaxVendorMessage = 300

// errorPeek is how much of an error answer the relay reads its type and
// message from, as it streams back: a vendor's error is a few hundred
// bytes.
const errorPeek = 16 << 10

func (m *Model) upstream(provider string) (string, bool) {
	ups := m.Upstreams
	if ups == nil {
		ups = DefaultModelUpstreams
	}
	u, ok := ups[provider]
	return u, ok && u != ""
}

// serveModel relays one /model/<provider>/<path> request.
func (p *Proxy) serveModel(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	m := p.Model
	rest := strings.TrimPrefix(req.URL.EscapedPath(), ModelPrefix)
	provider, path, _ := strings.Cut(rest, "/")
	call := ModelCall{Provider: provider, Method: req.Method, Path: "/" + path}
	done := func() {
		call.Latency = time.Since(start)
		if m != nil && m.Log != nil {
			m.Log(call)
		}
	}
	refuse := func(status int, why string) {
		call.Err = p.mask(why)
		done()
		writeJSON(w, status, map[string]any{"type": "error", "error": map[string]string{"type": "relay_error", "message": "relay: " + call.Err}})
	}
	if p.NoModel {
		w.Header().Set(egress.RefusedHeader, "model")
		refuse(http.StatusForbidden, "the kind's egress reaches no model")
		return
	}
	if m == nil {
		refuse(http.StatusNotFound, "this session reaches no model through the relay")
		return
	}
	base, ok := m.upstream(provider)
	if !ok {
		refuse(http.StatusNotFound, fmt.Sprintf("%q is not a model provider the relay reaches", provider))
		return
	}
	if dotSegment(path) {
		refuse(http.StatusBadRequest, "the path is not a model API path")
		return
	}
	if !m.pathAllowed(provider, "/"+path) {
		refuse(http.StatusForbidden, fmt.Sprintf("%s %s is not a model API path the node forwards", req.Method, "/"+path))
		return
	}
	var held []byte
	if req.Body != nil {
		b, err := io.ReadAll(io.LimitReader(req.Body, modelBodyMax+1))
		if err != nil {
			refuse(http.StatusBadRequest, "reading the request body: "+err.Error())
			return
		}
		if len(b) > modelBodyMax {
			refuse(http.StatusRequestEntityTooLarge, "the request body is over the relay's limit")
			return
		}
		held = b
	}
	if m.Inspect != nil && len(held) > 0 {
		if err := m.Inspect(held); err != nil {
			w.Header().Set(egress.ErrorHeader, egress.CodeExposed)
			refuse(http.StatusForbidden, err.Error())
			return
		}
	}
	target := strings.TrimRight(base, "/") + "/" + path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	rt := m.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	send := func(fresh bool) (*http.Response, error) {
		cred, err := m.Credential(req.Context(), provider, fresh)
		if err != nil {
			return nil, err
		}
		if cred.Header == "" || strings.TrimSpace(cred.Value) == "" {
			return nil, errors.New("the lease's model credential is empty")
		}
		p.remember(secretParts(cred.Value))
		out, err := http.NewRequestWithContext(req.Context(), req.Method, target, bytes.NewReader(held))
		if err != nil {
			return nil, err
		}
		out.ContentLength = int64(len(held))
		if len(held) == 0 {
			out.Body, out.GetBody = nil, nil
		}
		for k, v := range req.Header {
			if !modelDropped[http.CanonicalHeaderKey(k)] {
				out.Header[k] = append([]string(nil), v...)
			}
		}
		for _, k := range req.Header.Values("Connection") {
			for _, name := range strings.Split(k, ",") {
				out.Header.Del(strings.TrimSpace(name))
			}
		}
		out.Header.Set(cred.Header, cred.Value)
		return rt.RoundTrip(out)
	}
	resp, err := send(false)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		// nothing has gone back to the session: read the credential again
		// and ask once more
		resp.Body.Close()
		call.Refreshed = true
		resp, err = send(true)
	}
	if err != nil {
		if req.Context().Err() != nil {
			call.Err = "the session went away"
			done()
			return
		}
		status := http.StatusBadGateway
		if errors.Is(err, ErrModelRefused) {
			status = http.StatusForbidden
		}
		refuse(status, provider+": "+err.Error())
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		if !modelHeld[http.CanonicalHeaderKey(k)] {
			w.Header()[k] = append([]string(nil), v...)
		}
	}
	call.Status = resp.StatusCode
	w.WriteHeader(resp.StatusCode)
	var body io.Reader = resp.Body
	var peek *peekBuffer
	if resp.StatusCode >= http.StatusBadRequest {
		// the vendor's error type and message, read from what streams back
		peek = &peekBuffer{max: errorPeek}
		body = io.TeeReader(resp.Body, peek)
	}
	call.Bytes, err = streamBody(w, body)
	if err != nil && req.Context().Err() == nil {
		call.Err = p.mask(err.Error())
	}
	if peek != nil {
		typ, msg := VendorError(peek.Bytes(), resp.Header.Get("Content-Encoding"))
		call.ErrorType, call.ErrorMessage = clipRunes(p.mask(typ), 100), clipRunes(p.mask(msg), MaxVendorMessage)
	}
	done()
}

// peekBuffer keeps the first max bytes written to it and drops the rest.
type peekBuffer struct {
	max int
	buf bytes.Buffer
}

func (b *peekBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *peekBuffer) Bytes() []byte { return b.buf.Bytes() }

// VendorError is a model vendor's error type and message in an error
// answer's body (Content-Encoding gzip or none): Anthropic's
// {"type":"error","error":{"type":"permission_error","message":"..."}}
// or OpenAI's {"error":{"type":"...","code":"...","message":"..."}}.
// Both are blank for a body that is neither: nothing else of a body is
// ever read out of it.
func VendorError(body []byte, encoding string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return "", ""
		}
		// a truncated stream still yields what it held
		raw, _ := io.ReadAll(io.LimitReader(zr, errorPeek))
		body = raw
	default:
		return "", ""
	}
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || len(e.Error) == 0 {
		return "", ""
	}
	var inner struct {
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(e.Error, &inner) != nil {
		// an error that is a bare string
		var msg string
		if json.Unmarshal(e.Error, &msg) == nil {
			return "", strings.TrimSpace(msg)
		}
		return "", ""
	}
	typ := strings.TrimSpace(inner.Type)
	if typ == "" {
		var code string
		if json.Unmarshal(inner.Code, &code) == nil {
			typ = strings.TrimSpace(code)
		}
	}
	return typ, strings.TrimSpace(inner.Message)
}

// clipRunes is s bounded to n bytes, on a rune boundary, its control
// characters spaces, so a log line stays one line.
func clipRunes(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// streamBody copies body to w a read at a time, flushing each, so an
// event stream reaches the session as the model writes it.
func streamBody(w http.ResponseWriter, body io.Reader) (int64, error) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	var n int64
	for {
		k, rerr := body.Read(buf)
		if k > 0 {
			wrote, werr := w.Write(buf[:k])
			n += int64(wrote)
			if werr != nil {
				return n, werr
			}
			_ = rc.Flush()
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}

// ListenModel serves the model route alone on a loopback TCP port, for a
// session on the host, whose harness reaches its model over HTTP and
// cannot name a unix socket. The route sits under a capability, a
// random path segment only this lease's session is given, so no other
// process on the host can spend the lease's credential through it.
// base is the route's root, "http://127.0.0.1:<port>/<capability>";
// the session's base URL is base + ModelPrefix + provider. It closes
// with the proxy.
func (p *Proxy) ListenModel() (base string, err error) {
	var c [16]byte
	if _, err := rand.Read(c[:]); err != nil {
		return "", err
	}
	capability := "/" + hex.EncodeToString(c[:])
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		if len(path) < len(capability) || subtle.ConstantTimeCompare([]byte(path[:len(capability)]), []byte(capability)) != 1 ||
			!strings.HasPrefix(path[len(capability):], ModelPrefix) {
			http.NotFound(w, req)
			return
		}
		r2 := req.Clone(req.Context())
		r2.URL.Path = path[len(capability):]
		r2.URL.RawPath = ""
		if raw := req.URL.RawPath; raw != "" && strings.HasPrefix(raw, capability) {
			r2.URL.RawPath = raw[len(capability):]
		}
		p.serveModel(w, r2)
	})}
	p.mu.Lock()
	p.modelSrv = srv
	p.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.logf("sessionproxy: serve the model route: %v", err)
		}
	}()
	return "http://" + ln.Addr().String() + capability, nil
}

// ForwarderPaths are the paths the node's model forwarder sends on, per
// provider (projectmesh-io/api ADR 001 amendment "relay sidecar"): a
// conversation, its token count and the model list, for Anthropic and
// OpenAI. A path ending "/*" is that path and one segment under it.
var ForwarderPaths = map[string][]string{
	"anthropic": {"/v1/messages", "/v1/messages/count_tokens", "/v1/models", "/v1/models/*"},
	"openai":    {"/v1/responses", "/v1/chat/completions", "/v1/models", "/v1/models/*"},
}

// pathAllowed says whether path (escaped, leading slash, no query) is
// one m.Paths names for provider; with no Paths, every path is.
func (m *Model) pathAllowed(provider, path string) bool {
	if m.Paths == nil {
		return true
	}
	for _, p := range m.Paths[provider] {
		if prefix, ok := strings.CutSuffix(p, "/*"); ok {
			rest, under := strings.CutPrefix(path, prefix+"/")
			if under && rest != "" && !strings.Contains(rest, "/") {
				return true
			}
			continue
		}
		if path == p {
			return true
		}
	}
	return false
}
