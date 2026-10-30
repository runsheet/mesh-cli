package registryrelay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
)

// sessionToken is the lease's session token in the tests.
const sessionToken = "mesh-session.the-lease-s-own"

// fakeRegistry is a registry as runsheet's gateway/registry serves one:
// the OCI Distribution API at /v2/ (blob uploads in chunks, manifests by
// tag and digest) and binaries at /artifacts/, over a map. It takes a
// session's DPoP token on every route unless tokenOnly, when it answers
// 401 with Docker's Bearer challenge and its realm (/v2/token) exchanges
// the DPoP token, with a proof for the realm's URL, for a registry
// token. Every proof is checked by every rule of ADR 064.
type fakeRegistry struct {
	t         *testing.T
	jkt       string
	tokenOnly bool
	// expiresIn is the realm's token life, in seconds.
	expiresIn int
	// gate, set, is called on the first chunk of an upload and of a
	// blob download, so a test can prove the relay streams.
	gate func(what string)

	srv      *httptest.Server
	verifier dpoptest.Verifier
	mu       sync.Mutex
	blobs    map[string][]byte
	uploads  map[string]*bytes.Buffer
	tags     map[string]string
	files    map[string][]byte
	tokens   map[string]bool
	revoked  map[string]bool
	seq      int
	auth     []string // every route's Authorization scheme, in order
	exchange int
	scopes   []string
}

func newRegistry(t *testing.T, key *dpop.Key, tokenOnly bool) *fakeRegistry {
	g := &fakeRegistry{t: t, jkt: key.Thumbprint(), tokenOnly: tokenOnly, expiresIn: 300,
		blobs: map[string][]byte{}, uploads: map[string]*bytes.Buffer{}, tags: map[string]string{}, files: map[string][]byte{},
		tokens: map[string]bool{}, revoked: map[string]bool{}}
	g.srv = httptest.NewTLSServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (g *fakeRegistry) challenge(w http.ResponseWriter, scope string) {
	c := fmt.Sprintf(`Bearer realm="%s/v2/token",service="registry.test"`, g.srv.URL)
	if scope != "" {
		c += fmt.Sprintf(`,scope="%s"`, scope)
	}
	w.Header().Set("WWW-Authenticate", c)
	http.Error(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`, http.StatusUnauthorized)
}

// dpopOK checks r's DPoP token and its proof for r's own URL.
func (g *fakeRegistry) dpopOK(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "DPoP ")
	if !ok || tok != sessionToken {
		return false
	}
	err := g.verifier.Verify(r.Header.Get("DPoP"), dpoptest.Request{Method: r.Method, URL: g.srv.URL + r.URL.Path, Token: tok, JKT: g.jkt})
	if err != nil {
		g.t.Errorf("the registry refused a proof for %s %s: %v", r.Method, r.URL.Path, err)
	}
	return err == nil
}

func (g *fakeRegistry) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v2/token" {
		g.serveRealm(w, r)
		return
	}
	auth := r.Header.Get("Authorization")
	scheme, _, _ := strings.Cut(auth, " ")
	g.mu.Lock()
	g.auth = append(g.auth, scheme)
	g.mu.Unlock()
	switch {
	case strings.HasPrefix(auth, "Bearer "):
		tok := strings.TrimPrefix(auth, "Bearer ")
		g.mu.Lock()
		ok := g.tokens[tok] && !g.revoked[tok]
		g.mu.Unlock()
		if !ok {
			g.challenge(w, "")
			return
		}
	case !g.tokenOnly && g.dpopOK(r):
	default:
		g.challenge(w, "")
		return
	}
	switch {
	case r.URL.Path == "/v2/":
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(r.URL.Path, "/artifacts/"):
		g.serveArtifact(w, r)
	default:
		g.serveOCI(w, r)
	}
}

func (g *fakeRegistry) serveRealm(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "DPoP ")
	if !ok || tok != sessionToken {
		http.Error(w, "present the session's DPoP token", http.StatusUnauthorized)
		return
	}
	if err := g.verifier.Verify(r.Header.Get("DPoP"), dpoptest.Request{Method: r.Method, URL: g.srv.URL + r.URL.Path, Token: tok, JKT: g.jkt}); err != nil {
		g.t.Errorf("the realm refused a proof: %v", err)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	g.mu.Lock()
	g.exchange++
	g.seq++
	reg := fmt.Sprintf("rsreg_%d", g.seq)
	g.tokens[reg] = true
	g.scopes = append(g.scopes, r.URL.Query().Get("scope"))
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"token": reg, "access_token": reg, "expires_in": g.expiresIn})
}

func (g *fakeRegistry) serveOCI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	switch {
	case strings.Contains(path, "/blobs/uploads/"):
		name, id, _ := strings.Cut(path, "/blobs/uploads/")
		switch r.Method {
		case http.MethodPost:
			g.mu.Lock()
			g.seq++
			id = fmt.Sprint(g.seq)
			g.uploads[id] = &bytes.Buffer{}
			g.mu.Unlock()
			// an absolute Location, as some registries answer
			w.Header().Set("Location", g.srv.URL+"/v2/"+name+"/blobs/uploads/"+id)
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPatch, http.MethodPut:
			g.mu.Lock()
			buf := g.uploads[id]
			g.mu.Unlock()
			if buf == nil {
				http.Error(w, "no upload", http.StatusNotFound)
				return
			}
			first := true
			chunk := make([]byte, 16<<10)
			for {
				n, err := r.Body.Read(chunk)
				if n > 0 {
					g.mu.Lock()
					buf.Write(chunk[:n])
					g.mu.Unlock()
					if first && g.gate != nil {
						first = false
						g.gate("upload")
					}
				}
				if err != nil {
					break
				}
			}
			if r.Method == http.MethodPatch {
				w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+id)
				w.WriteHeader(http.StatusAccepted)
				return
			}
			g.mu.Lock()
			data := buf.Bytes()
			d := r.URL.Query().Get("digest")
			if digestOf(data) != d {
				g.mu.Unlock()
				http.Error(w, "digest mismatch", http.StatusBadRequest)
				return
			}
			g.blobs[d] = append([]byte(nil), data...)
			delete(g.uploads, id)
			g.mu.Unlock()
			w.Header().Set("Location", "/v2/"+name+"/blobs/"+d)
			w.Header().Set("Docker-Content-Digest", d)
			w.WriteHeader(http.StatusCreated)
		}
	case strings.Contains(path, "/blobs/"):
		_, d, _ := strings.Cut(path, "/blobs/")
		g.mu.Lock()
		data, ok := g.blobs[d]
		g.mu.Unlock()
		if !ok {
			http.Error(w, "blob unknown", http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", d)
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if r.Method == http.MethodHead {
			return
		}
		half := len(data) / 2
		w.Write(data[:half])
		w.(http.Flusher).Flush()
		if g.gate != nil {
			g.gate("download")
		}
		w.Write(data[half:])
	case strings.Contains(path, "/manifests/"):
		name, ref, _ := strings.Cut(path, "/manifests/")
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			d := digestOf(body)
			g.mu.Lock()
			g.blobs[d] = body
			g.tags[name+":"+ref] = d
			g.mu.Unlock()
			w.Header().Set("Docker-Content-Digest", d)
			w.Header().Set("Location", "/v2/"+name+"/manifests/"+d)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet, http.MethodHead:
			g.mu.Lock()
			d := ref
			if !strings.HasPrefix(ref, "sha256:") {
				d = g.tags[name+":"+ref]
			}
			body, ok := g.blobs[d]
			g.mu.Unlock()
			if !ok {
				http.Error(w, "manifest unknown", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", d)
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if r.Method == http.MethodGet {
				w.Write(body)
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeRegistry) serveArtifact(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.files[r.URL.Path] = body
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		g.mu.Lock()
		body, ok := g.files[r.URL.Path]
		g.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}
}

func (g *fakeRegistry) schemes() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.auth...)
}

func (g *fakeRegistry) exchanges() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.exchange
}

// relayRig is a relay for g on the session's loopback, its address what
// MESH_REGISTRY names.
func relayRig(t *testing.T, g *fakeRegistry, key *dpop.Key) (*Relay, string) {
	t.Helper()
	r := &Relay{Upstream: g.srv.URL, HTTP: g.srv.Client(), Key: key, Logf: t.Logf,
		Token: func(context.Context) (string, error) { return sessionToken, nil }}
	front := httptest.NewServer(r)
	t.Cleanup(front.Close)
	return r, strings.TrimPrefix(front.URL, "http://")
}

// crane is a registry client as crane, docker and oras are: it holds no
// credential, follows a 401's Bearer challenge to its realm for a token
// and sends again, and follows each upload's Location.
type crane struct {
	t     *testing.T
	addr  string // MESH_REGISTRY
	token string
	sent  []string // every Authorization it sent
}

func (c *crane) do(method, path string, body func() io.Reader, header map[string]string) *http.Response {
	c.t.Helper()
	for try := 0; try < 2; try++ {
		var rd io.Reader
		if body != nil {
			rd = body()
		}
		url := path
		if !strings.HasPrefix(url, "http") {
			url = "http://" + c.addr + path
		}
		req, err := http.NewRequest(method, url, rd)
		if err != nil {
			c.t.Fatal(err)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		c.sent = append(c.sent, req.Header.Get("Authorization"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			c.t.Fatal(err)
		}
		if resp.StatusCode != http.StatusUnauthorized || try == 1 {
			return resp
		}
		_, params := parseChallenge(resp.Header.Get("WWW-Authenticate"))
		resp.Body.Close()
		if !strings.HasPrefix(params["realm"], "http://"+c.addr+"/") {
			c.t.Fatalf("the challenge's realm %q is not the relay's", params["realm"])
		}
		tr, err := http.Get(params["realm"] + "?service=" + params["service"])
		if err != nil {
			c.t.Fatal(err)
		}
		var tok struct{ Token string }
		json.NewDecoder(tr.Body).Decode(&tok)
		tr.Body.Close()
		c.token = tok.Token
	}
	panic("unreachable")
}

func (c *crane) expect(resp *http.Response, status int) []byte {
	c.t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status {
		c.t.Fatalf("%s %s answered %d, want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, status, b)
	}
	return b
}

// pushBlob pushes data as crane does: HEAD, POST an upload, PATCH the
// bytes, PUT ?digest=.
func (c *crane) pushBlob(name string, data []byte) string {
	d := digestOf(data)
	c.expect(c.do(http.MethodHead, "/v2/"+name+"/blobs/"+d, nil, nil), http.StatusNotFound)
	resp := c.do(http.MethodPost, "/v2/"+name+"/blobs/uploads/", nil, nil)
	c.expect(resp, http.StatusAccepted)
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/v2/") {
		c.t.Fatalf("the upload's Location %q is not on the relay", loc)
	}
	resp = c.do(http.MethodPatch, loc, func() io.Reader { return bytes.NewReader(data) }, map[string]string{"Content-Type": "application/octet-stream"})
	c.expect(resp, http.StatusAccepted)
	loc = resp.Header.Get("Location")
	c.expect(c.do(http.MethodPut, loc+"?digest="+d, nil, nil), http.StatusCreated)
	return d
}

// The relay serves a registry that takes the session's DPoP token: a
// crane-like client with no credential pushes a blob and a manifest and
// pulls them back, and binaries go up and down /artifacts/; every
// request reached the registry in the DPoP scheme with a proof for its
// own URL, and nothing the client sent did.
func TestPushAndPullThroughTheRelayOnTheSessionToken(t *testing.T) {
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	g := newRegistry(t, key, false)
	_, addr := relayRig(t, g, key)
	c := &crane{t: t, addr: addr}
	pushAndPull(t, c)
	for _, s := range g.schemes() {
		if s != "DPoP" {
			t.Fatalf("the registry was reached in the %q scheme: %v", s, g.schemes())
		}
	}
	if g.exchanges() != 0 {
		t.Fatalf("%d token exchanges with a registry that takes the session token", g.exchanges())
	}
}

// The relay serves a registry that answers Docker's challenge: it
// exchanges the session token, with a DPoP proof, at the registry's
// realm, sends again with the registry token it got, keeps it by scope,
// and exchanges again once it is near its end. The client never sees a
// token of the registry's or the session's.
func TestPushAndPullThroughTheRelayByDockersTokenFlow(t *testing.T) {
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	g := newRegistry(t, key, true)
	r, addr := relayRig(t, g, key)
	now := time.Now()
	var clock sync.Mutex
	r.Now = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
	c := &crane{t: t, addr: addr}
	pushAndPull(t, c)
	if n := g.exchanges(); n == 0 {
		t.Fatal("the relay never exchanged at the realm")
	}
	g.mu.Lock()
	scopes := append([]string(nil), g.scopes...)
	g.mu.Unlock()
	wantScope := "repository:acme/web/app:pull,push"
	found := false
	for _, s := range scopes {
		found = found || s == wantScope
	}
	if !found {
		t.Fatalf("the relay asked the realm for %q, not %q", scopes, wantScope)
	}
	for _, sent := range c.sent {
		if sent != "" && sent != "Bearer "+placeholder {
			t.Fatalf("the client held a credential: %q", sent)
		}
	}
	// a kept token is used until near its end, then exchanged again
	before := g.exchanges()
	c.expect(c.do(http.MethodGet, "/v2/acme/web/app/manifests/x", nil, nil), http.StatusOK)
	if g.exchanges() != before {
		t.Fatal("a kept token was exchanged again")
	}
	clock.Lock()
	now = now.Add(time.Duration(g.expiresIn)*time.Second - time.Second)
	clock.Unlock()
	c.expect(c.do(http.MethodGet, "/v2/acme/web/app/manifests/x", nil, nil), http.StatusOK)
	if g.exchanges() != before+1 {
		t.Fatalf("a token near its end was not exchanged again: %d exchanges, want %d", g.exchanges(), before+1)
	}
	// a token the registry refuses is dropped, exchanged anew, and the
	// bodiless request sent again
	g.mu.Lock()
	for tok := range g.tokens {
		g.revoked[tok] = true
	}
	g.mu.Unlock()
	c.expect(c.do(http.MethodGet, "/v2/acme/web/app/manifests/x", nil, nil), http.StatusOK)
	if g.exchanges() != before+2 {
		t.Fatalf("a refused token was not exchanged again: %d exchanges", g.exchanges())
	}
}

// pushAndPull is crane push and pull of one image, and a binary.
func pushAndPull(t *testing.T, c *crane) {
	t.Helper()
	c.expect(c.do(http.MethodGet, "/v2/", nil, nil), http.StatusOK)
	layer := bytes.Repeat([]byte("layer!"), 50_000)
	ld := c.pushBlob("acme/web/app", layer)
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	cd := c.pushBlob("acme/web/app", config)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":%q,"size":%d},"layers":[{"digest":%q,"size":%d}]}`,
		cd, len(config), ld, len(layer)))
	resp := c.do(http.MethodPut, "/v2/acme/web/app/manifests/x", func() io.Reader { return bytes.NewReader(manifest) },
		map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
	c.expect(resp, http.StatusCreated)
	if resp.Header.Get("Docker-Content-Digest") != digestOf(manifest) {
		t.Fatalf("the manifest's digest %q", resp.Header.Get("Docker-Content-Digest"))
	}
	if got := c.expect(c.do(http.MethodGet, "/v2/acme/web/app/manifests/x", nil, nil), http.StatusOK); !bytes.Equal(got, manifest) {
		t.Fatal("the manifest pulled is not the one pushed")
	}
	if got := c.expect(c.do(http.MethodGet, "/v2/acme/web/app/blobs/"+ld, nil, nil), http.StatusOK); !bytes.Equal(got, layer) {
		t.Fatal("the layer pulled is not the one pushed")
	}
	head := c.do(http.MethodHead, "/v2/acme/web/app/blobs/"+cd, nil, nil)
	c.expect(head, http.StatusOK)
	if head.Header.Get("Content-Length") != fmt.Sprint(len(config)) {
		t.Fatalf("HEAD's length %q", head.Header.Get("Content-Length"))
	}
	bin := []byte("#!/bin/sh\necho mesh\n")
	c.expect(c.do(http.MethodPut, "/artifacts/acme/web/cli/1.0.0/cli", func() io.Reader { return bytes.NewReader(bin) }, nil), http.StatusCreated)
	if got := c.expect(c.do(http.MethodGet, "/artifacts/acme/web/cli/1.0.0/cli", nil, nil), http.StatusOK); !bytes.Equal(got, bin) {
		t.Fatal("the binary pulled is not the one put")
	}
}

// Blob bodies stream through the relay both ways: the registry has the
// first bytes of an upload before the client has sent its last, and the
// client has the first bytes of a download before the registry has
// sent its last. A relay that buffered would wait out the gate.
func TestBlobsStreamThroughTheRelay(t *testing.T) {
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	g := newRegistry(t, key, false)
	_, addr := relayRig(t, g, key)
	c := &crane{t: t, addr: addr}
	reached := make(chan string, 2)
	release := make(chan struct{})
	g.gate = func(what string) {
		reached <- what
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}
	resp := c.do(http.MethodPost, "/v2/acme/web/app/blobs/uploads/", nil, nil)
	c.expect(resp, http.StatusAccepted)
	loc := resp.Header.Get("Location")

	first, rest := bytes.Repeat([]byte("a"), 64<<10), bytes.Repeat([]byte("b"), 64<<10)
	pr, pw := io.Pipe()
	go func() {
		pw.Write(first)
		select {
		case w := <-reached:
			if w != "upload" {
				t.Errorf("the gate heard %s", w)
			}
		case <-time.After(5 * time.Second):
			t.Error("the registry had none of the upload before the client sent the rest: the relay buffers")
		}
		close(release)
		pw.Write(rest)
		pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPatch, "http://"+addr+loc, pr)
	up, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	up.Body.Close()
	if up.StatusCode != http.StatusAccepted {
		t.Fatalf("the streamed upload answered %d", up.StatusCode)
	}
	all := append(append([]byte(nil), first...), rest...)
	c.expect(c.do(http.MethodPut, up.Header.Get("Location")+"?digest="+digestOf(all), nil, nil), http.StatusCreated)

	release = make(chan struct{})
	resp = c.do(http.MethodGet, "/v2/acme/web/app/blobs/"+digestOf(all), nil, nil)
	defer resp.Body.Close()
	got := make([]byte, len(all)/2)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, got); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client had none of the download before the registry sent the rest: the relay buffers")
	}
	if w := <-reached; w != "download" {
		t.Fatalf("the gate heard %s", w)
	}
	close(release)
	tail, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(append(got, tail...), all) {
		t.Fatal("the streamed blob is not the one pushed")
	}
}

// The relay sends the session token to no realm but the registry's own,
// and answers its own realm with a placeholder that is no credential.
func TestTheRelaySendsTheTokenToNoOtherRealm(t *testing.T) {
	key, _ := dpop.NewKey()
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the session token went to another host: %s %s", r.Method, r.URL)
	}))
	defer elsewhere.Close()
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="x"`, elsewhere.URL))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer reg.Close()
	r := &Relay{Upstream: reg.URL, HTTP: reg.Client(), Key: key, Logf: t.Logf,
		Token: func(context.Context) (string, error) { return sessionToken, nil }}
	front := httptest.NewServer(r)
	defer front.Close()
	resp, err := http.Get(front.URL + "/v2/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || r.realm != nil {
		t.Fatalf("answered %d, realm kept %v", resp.StatusCode, r.realm)
	}
	resp, err = http.Get(front.URL + TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	var tok struct{ Token string }
	json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if tok.Token != placeholder {
		t.Fatalf("the relay's realm answered %q", tok.Token)
	}
	for _, path := range []string{"/mcp/x", "/git/x", "/"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s answered %d", path, rec.Code)
		}
	}
}

func TestScopeOf(t *testing.T) {
	for _, tc := range []struct{ path, method, want string }{
		{"/v2/", http.MethodGet, ""},
		{"/v2/acme/web/app/manifests/x", http.MethodGet, "repository:acme/web/app:pull"},
		{"/v2/acme/web/app/blobs/uploads/", http.MethodPost, "repository:acme/web/app:pull,push"},
		{"/v2/acme/web/a/b/blobs/sha256:00", http.MethodHead, "repository:acme/web/a/b:pull"},
		{"/v2/acme/web/app/tags/list", http.MethodGet, "repository:acme/web/app:pull"},
		{"/artifacts/acme/web/cli/1.0.0/cli", http.MethodPut, "repository:acme/web/cli:pull,push"},
		{"/artifacts/acme/web", http.MethodGet, ""},
	} {
		if got := scopeOf(tc.path, tc.method); got != tc.want {
			t.Errorf("%s %s: %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

// A body the relay sent before the registry first challenged is never
// sent twice: the challenge goes back to the client with the relay's own
// realm, the client's retry carries the placeholder, and the relay sends
// it with a registry token it exchanged.
func TestAChallengedBodyIsPassedBackNotReplayed(t *testing.T) {
	key, _ := dpop.NewKey()
	g := newRegistry(t, key, true)
	_, addr := relayRig(t, g, key)
	c := &crane{t: t, addr: addr}
	bin := []byte("first thing sent")
	c.expect(c.do(http.MethodPut, "/artifacts/acme/web/cli/1.0.0/cli", func() io.Reader { return bytes.NewReader(bin) }, nil), http.StatusCreated)
	if len(c.sent) != 2 || c.sent[0] != "" || c.sent[1] != "Bearer "+placeholder {
		t.Fatalf("the client sent %q", c.sent)
	}
	if s := g.schemes(); len(s) != 2 || s[0] != "DPoP" || s[1] != "Bearer" {
		t.Fatalf("the registry heard %v", s)
	}
	g.mu.Lock()
	got := g.files["/artifacts/acme/web/cli/1.0.0/cli"]
	g.mu.Unlock()
	if !bytes.Equal(got, bin) {
		t.Fatalf("the registry holds %q", got)
	}
}
