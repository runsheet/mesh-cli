package sessionproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
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
	// Transport sends each request; http.DefaultTransport when nil. A
	// proxy follows no redirect, so it is a RoundTripper.
	Transport http.RoundTripper
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
}

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
	call.Bytes, err = streamBody(w, resp.Body)
	if err != nil && req.Context().Err() == nil {
		call.Err = p.mask(err.Error())
	}
	done()
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
