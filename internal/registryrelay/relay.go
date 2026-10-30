// Package registryrelay serves a lease's artifact registry to its
// session through the relay (projectmesh-io/api ADR 001 amendment,
// registry through the relay; mesh-specs gap 18). The brief names the
// registry (`registry: {base_url}`); the relay serves it on the
// session's loopback, which the session knows as MESH_REGISTRY
// (127.0.0.1:<port>): the OCI Distribution API at /v2/ and binaries at
// /artifacts/, each request forwarded to the same path under base_url.
//
// The session holds no registry credential, and none reaches it. Any
// Authorization the session sends is dropped, and the relay adds the
// lease's own:
//
//   - the lease's session token in the DPoP scheme, `Authorization: DPoP
//     <token>` with a fresh proof for the upstream URL, which a registry
//     like runsheet's takes on every route; or
//   - once the registry answers Docker's 401 challenge, `Bearer
//     <registry token>`: the relay exchanges the session token, with a
//     DPoP proof for the realm's URL, at the challenge's realm — which
//     must be the registry's own origin — and keeps the token by scope,
//     exchanging again before it lapses.
//
// Bodies stream both ways without buffering, so a request with a body
// is never sent twice: once the registry has challenged, the relay
// exchanges before it sends one; a challenge to a body the relay sent
// first is passed back to the client with its realm pointed at the
// relay's own TokenPath, which answers a placeholder, and the client's
// retry is sent with a bearer. A Location naming the registry's origin
// is rewritten to the loopback, so the client keeps talking to the
// relay.
package registryrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The routes the relay serves.
const (
	// Prefix is the OCI Distribution API's.
	Prefix = "/v2/"
	// ArtifactsPrefix is the binaries'.
	ArtifactsPrefix = "/artifacts/"
	// TokenPath is the relay's own realm: a placeholder token, which
	// the relay never sends upstream.
	TokenPath = "/v2/token"
)

// Env names the relay's registry address in the session's environment.
const Env = "MESH_REGISTRY"

// placeholder is the token the relay's realm answers: no credential.
const placeholder = "mesh-relay"

// Serves says the relay answers path.
func Serves(path string) bool {
	return path == "/v2" || strings.HasPrefix(path, Prefix) || strings.HasPrefix(path, ArtifactsPrefix)
}

// Signer signs one request's DPoP proof (internal/dpop.Key).
type Signer interface {
	Proof(method, rawURL, token string) (string, error)
}

// Relay is one lease's registry.
type Relay struct {
	// Upstream is the registry's https origin, from the brief.
	Upstream string
	// HTTP reaches the registry: the node's client in process, the
	// lease's tunnel in a relay sidecar. Its redirects are never
	// followed: the client sees them, rewritten.
	HTTP *http.Client
	// Token is the lease's session token of the moment, and Key its
	// DPoP key.
	Token func(ctx context.Context) (string, error)
	Key   Signer
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logf takes the relay's lines: never a token.
	Logf func(format string, args ...any)

	mu sync.Mutex
	// realm is the registry's challenge, once it has made one; tokens
	// the registry tokens by scope.
	realm  *challenge
	tokens map[string]registryToken
}

// challenge is a registry's Bearer challenge.
type challenge struct {
	realm, service string
}

// registryToken is a token the realm answered, and when it lapses.
type registryToken struct {
	value   string
	expires time.Time
}

func (r *Relay) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Relay) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Relay) client() *http.Client {
	c := http.DefaultClient
	if r.HTTP != nil {
		c = r.HTTP
	}
	cp := *c
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}

// hopHeaders are never forwarded, nor the session's own credentials.
var hopHeaders = []string{
	"Authorization", "Proxy-Authorization", "Proxy-Authenticate", "Cookie", "Dpop",
	"Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Proxy-Connection",
}

// ServeHTTP forwards one request of the session's to the registry.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == TokenPath {
		r.serveRealm(w)
		return
	}
	if !Serves(req.URL.Path) {
		http.NotFound(w, req)
		return
	}
	base, err := url.Parse(strings.TrimRight(r.Upstream, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		http.Error(w, "relay: the lease's registry is not a url", http.StatusBadGateway)
		return
	}
	target := *base
	target.Path, target.RawPath = req.URL.Path, req.URL.RawPath
	target.RawQuery = req.URL.RawQuery
	scope := scopeOf(req.URL.Path, req.Method)
	replayable := req.Body == nil || req.Body == http.NoBody || req.ContentLength == 0
	if replayable && req.Body != nil {
		req.Body.Close()
	}

	resp, bearer, err := r.send(req, target.String(), scope, false)
	if err != nil {
		r.logf("registryrelay: %s %s: %v", req.Method, req.URL.Path, err)
		http.Error(w, "relay: the registry could not be reached", http.StatusBadGateway)
		return
	}
	if resp.StatusCode == http.StatusUnauthorized {
		ch, ok := r.challenged(resp, base)
		switch {
		case ok && replayable:
			// a bodiless request is sent again, now with a registry
			// token: a token that was refused is exchanged anew
			if bearer {
				r.forget(scope)
			}
			if s := ch.scope; s != "" {
				scope = s
			}
			resp.Body.Close()
			resp, _, err = r.send(req, target.String(), scope, true)
			if err != nil {
				r.logf("registryrelay: %s %s: %v", req.Method, req.URL.Path, err)
				http.Error(w, "relay: the registry could not be reached", http.StatusBadGateway)
				return
			}
		case ok && bearer:
			r.forget(scope)
		}
	}
	defer resp.Body.Close()
	r.answer(w, req, resp, base)
}

// send sends req to target on the lease's credential: a registry token
// when the registry has challenged (or must), else the session token in
// the DPoP scheme. bearer says which went.
func (r *Relay) send(req *http.Request, target, scope string, mustBearer bool) (*http.Response, bool, error) {
	ctx := req.Context()
	out, err := http.NewRequestWithContext(ctx, req.Method, target, req.Body)
	if err != nil {
		return nil, false, err
	}
	if req.Body == http.NoBody || req.ContentLength == 0 {
		out.Body, out.ContentLength = nil, 0
	} else {
		out.ContentLength = req.ContentLength
	}
	for k, vs := range req.Header {
		out.Header[k] = append([]string(nil), vs...)
	}
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	r.mu.Lock()
	realm := r.realm
	r.mu.Unlock()
	bearer := realm != nil || mustBearer
	if bearer {
		if realm == nil {
			return nil, false, errors.New("the registry asked for a token and named no realm")
		}
		tok, err := r.registryToken(ctx, realm, scope)
		if err != nil {
			return nil, false, err
		}
		out.Header.Set("Authorization", "Bearer "+tok)
	} else if err := r.dpop(ctx, out); err != nil {
		return nil, false, err
	}
	resp, err := r.client().Do(out)
	return resp, bearer, err
}

// dpop puts the session token on out in the DPoP scheme, with a fresh
// proof for out's method and URL.
func (r *Relay) dpop(ctx context.Context, out *http.Request) error {
	if r.Token == nil || r.Key == nil {
		return errors.New("the lease has no session token for its registry")
	}
	tok, err := r.Token(ctx)
	if err != nil {
		return fmt.Errorf("the lease's session token: %w", err)
	}
	proof, err := r.Key.Proof(out.Method, out.URL.String(), tok)
	if err != nil {
		return err
	}
	out.Header.Set("Authorization", "DPoP "+tok)
	out.Header.Set("DPoP", proof)
	return nil
}

// registryToken is a registry token for scope: the one kept while it
// has a fifth of its life left (and at least 5 s), else one exchanged
// at the realm now.
func (r *Relay) registryToken(ctx context.Context, ch *challenge, scope string) (string, error) {
	now := r.now()
	r.mu.Lock()
	t, ok := r.tokens[scope]
	r.mu.Unlock()
	if ok && now.Before(t.expires) {
		return t.value, nil
	}
	realm, err := url.Parse(ch.realm)
	if err != nil {
		return "", err
	}
	q := realm.Query()
	if ch.service != "" {
		q.Set("service", ch.service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if err := r.dpop(ctx, req); err != nil {
		return "", err
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("the registry's realm: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("the registry's realm answered %d", resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return "", fmt.Errorf("the registry's realm: %w", err)
	}
	tok := body.Token
	if tok == "" {
		tok = body.AccessToken
	}
	if tok == "" {
		return "", errors.New("the registry's realm answered no token")
	}
	life := time.Duration(body.ExpiresIn) * time.Second
	if life <= 0 {
		life = 60 * time.Second // Docker's default
	}
	early := max(life/5, 5*time.Second)
	if early >= life {
		early = life / 2
	}
	r.mu.Lock()
	if r.tokens == nil {
		r.tokens = map[string]registryToken{}
	}
	r.tokens[scope] = registryToken{value: tok, expires: now.Add(life - early)}
	r.mu.Unlock()
	return tok, nil
}

// forget drops scope's token, which the registry refused.
func (r *Relay) forget(scope string) {
	r.mu.Lock()
	delete(r.tokens, scope)
	r.mu.Unlock()
}

// challengeOf is a 401's Bearer challenge.
type challengeOf struct {
	challenge
	scope string
}

// challenged reads resp's Bearer challenge and keeps its realm, when
// the realm is the registry's own origin: the relay sends the session
// token nowhere else.
func (r *Relay) challenged(resp *http.Response, base *url.URL) (challengeOf, bool) {
	for _, v := range resp.Header.Values("Www-Authenticate") {
		scheme, params := parseChallenge(v)
		if !strings.EqualFold(scheme, "Bearer") || params["realm"] == "" {
			continue
		}
		u, err := url.Parse(params["realm"])
		if err != nil || u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) {
			r.logf("registryrelay: the registry's realm is not its own origin; no token is exchanged there")
			return challengeOf{}, false
		}
		ch := challengeOf{challenge: challenge{realm: params["realm"], service: params["service"]}, scope: params["scope"]}
		r.mu.Lock()
		r.realm = &ch.challenge
		r.mu.Unlock()
		return ch, true
	}
	return challengeOf{}, false
}

// answer copies resp to w: its body streamed, a Location on the
// registry's origin made relative, and a challenge's realm pointed at
// the relay's own.
func (r *Relay) answer(w http.ResponseWriter, req *http.Request, resp *http.Response, base *url.URL) {
	for k, vs := range resp.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Set-Cookie":
			continue
		case "Location":
			for _, v := range vs {
				w.Header().Add(k, relativeTo(v, base))
			}
			continue
		case "Www-Authenticate":
			for _, v := range vs {
				w.Header().Add(k, localRealm(v, req.Host))
			}
			continue
		}
		w.Header()[k] = append([]string(nil), vs...)
	}
	w.WriteHeader(resp.StatusCode)
	if req.Method == http.MethodHead {
		return
	}
	flush := func() {}
	if f, ok := w.(http.Flusher); ok {
		flush = f.Flush
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flush()
		}
		if err != nil {
			return
		}
	}
}

// serveRealm answers the relay's own realm: a placeholder the relay
// drops, so a client that followed a passed-back challenge retries.
func (r *Relay) serveRealm(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"token": placeholder, "access_token": placeholder, "expires_in": 300})
}

// relativeTo is loc with the registry's origin dropped, so the client
// follows it on the loopback.
func relativeTo(loc string, base *url.URL) string {
	u, err := url.Parse(loc)
	if err != nil || !u.IsAbs() {
		return loc
	}
	if u.Scheme == base.Scheme && strings.EqualFold(u.Host, base.Host) {
		u.Scheme, u.Host, u.User = "", "", nil
		return u.String()
	}
	return loc
}

// localRealm is a challenge with its realm the relay's own.
func localRealm(v, host string) string {
	scheme, params := parseChallenge(v)
	if !strings.EqualFold(scheme, "Bearer") {
		return v
	}
	params["realm"] = "http://" + host + TokenPath
	var parts []string
	for _, k := range []string{"realm", "service", "scope", "error"} {
		if val, ok := params[k]; ok {
			parts = append(parts, k+"="+strconv.Quote(val))
		}
	}
	return "Bearer " + strings.Join(parts, ",")
}

// scopeOf is the registry scope a request needs: the repository its
// path names, pull to read and pull,push to write; "" for none.
func scopeOf(path, method string) string {
	name := repository(path)
	if name == "" {
		return ""
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return "repository:" + name + ":pull"
	}
	return "repository:" + name + ":pull,push"
}

// repository is the repository a /v2/ or /artifacts/ path names.
func repository(path string) string {
	if rest, ok := strings.CutPrefix(path, ArtifactsPrefix); ok {
		parts := strings.SplitN(rest, "/", 4)
		if len(parts) < 3 {
			return ""
		}
		return strings.Join(parts[:3], "/")
	}
	rest, ok := strings.CutPrefix(path, Prefix)
	if !ok {
		return ""
	}
	for _, mark := range []string{"/manifests/", "/blobs/", "/tags/", "/referrers/"} {
		if i := strings.Index(rest, mark); i > 0 {
			return rest[:i]
		}
	}
	return ""
}

// parseChallenge reads one WWW-Authenticate value: its scheme and its
// quoted or bare parameters.
func parseChallenge(v string) (string, map[string]string) {
	v = strings.TrimSpace(v)
	scheme, rest, _ := strings.Cut(v, " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(rest[:eq]))
		rest = strings.TrimSpace(rest[eq+1:])
		var val string
		if strings.HasPrefix(rest, `"`) {
			end := 1
			var b strings.Builder
			for end < len(rest) && rest[end] != '"' {
				if rest[end] == '\\' && end+1 < len(rest) {
					end++
				}
				b.WriteByte(rest[end])
				end++
			}
			val = b.String()
			rest = rest[min(end+1, len(rest)):]
		} else {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				end = len(rest)
			}
			val = strings.TrimSpace(rest[:end])
			rest = rest[end:]
		}
		params[key] = val
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ","))
	}
	return scheme, params
}
