package egress

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
	"projectmesh.io/mesh-cli/internal/sockettest"
)

const registryHost = "api.runsheet.dev"

var registryPrefixes = []string{"https://api.runsheet.dev/npm/", "https://api.runsheet.dev/v2/", "https://api.runsheet.dev/artifacts/"}

// A lease's CA mints a certificate for a host its rules name, which a
// client trusting the CA alone takes; an hour long, kept until near its
// end and then minted again; never for a host past its constraints, and
// a certificate its key signs for another host is refused by the
// constraint itself.
func TestTheLeaseCAMintsShortLivedCertificatesForItsHostsAlone(t *testing.T) {
	if _, err := NewCA("l", nil, 0); err == nil {
		t.Fatal("a CA naming no hosts was made")
	}
	ca, err := NewCA("lse_1", []string{"API.runsheet.dev."}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ca.CertPEM(), []byte("PRIVATE")) || !strings.Contains(ca.String(), "api.runsheet.dev") {
		t.Fatalf("the CA's certificate %q / %s", ca.CertPEM(), ca)
	}
	c := ca.Certificate()
	if !c.IsCA || !c.PermittedDNSDomainsCritical || len(c.PermittedDNSDomains) != 1 || c.PermittedDNSDomains[0] != registryHost ||
		c.NotAfter.Sub(c.NotBefore) > CALifetime+time.Hour {
		t.Fatalf("the CA %+v", c)
	}
	pool := x509.NewCertPool()
	pool.AddCert(c)
	leaf, err := ca.Leaf(registryHost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: registryHost}); err != nil {
		t.Fatalf("the minted certificate does not verify: %v", err)
	}
	if life := leaf.Leaf.NotAfter.Sub(leaf.Leaf.NotBefore); life > LeafLifetime+backdate || life < LeafLifetime {
		t.Fatalf("a leaf lives %s", life)
	}
	again, _ := ca.Leaf(registryHost)
	if again != leaf {
		t.Fatal("a live leaf was minted again")
	}
	ca.Now = func() time.Time { return time.Now().Add(LeafLifetime - LeafRenewBefore/2) }
	if renewed, _ := ca.Leaf(registryHost); renewed == leaf {
		t.Fatal("a leaf near its end was not minted again")
	}
	for _, h := range []string{"registry.npmjs.org", "runsheet.dev", "127.0.0.1"} {
		if _, err := ca.Leaf(h); err == nil {
			t.Fatalf("a leaf for %s was minted", h)
		}
	}
	// the key itself cannot name another host to a client
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "evil.example"}, DNSNames: []string{"evil.example"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	evil, _ := x509.ParseCertificate(der)
	if _, err := evil.Verify(x509.VerifyOptions{Roots: pool, DNSName: "evil.example"}); err == nil {
		t.Fatal("a certificate past the CA's constraints verified")
	}
}

// A prefix takes the URLs under it at a segment boundary, on its own
// scheme, host and port, its path escaped as sent; a dot segment is
// never signed.
func TestARegistryPrefixMatchesAtItsBoundaryOnly(t *testing.T) {
	r, err := NewSignRule(append(registryPrefixes, "https://files.example:8443/pkg"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string]string{
		"https://api.runsheet.dev/npm/@anythink%2fshell":          "https://api.runsheet.dev/npm/",
		"https://api.runsheet.dev/npm/@anythink/shell/-/s.tgz":    "https://api.runsheet.dev/npm/",
		"https://API.runsheet.dev:443/v2/acme/web/blobs/sha256:1": "https://api.runsheet.dev/v2/",
		"https://api.runsheet.dev/artifacts/a/b/c/1.0/f?x=1":      "https://api.runsheet.dev/artifacts/",
		"https://api.runsheet.dev/npmx/left-pad":                  "",
		"https://api.runsheet.dev/npm":                            "",
		"https://api.runsheet.dev/mcp":                            "",
		"https://api.runsheet.dev/npm/../mcp":                     "",
		"https://api.runsheet.dev/npm/%2e%2e/mcp":                 "",
		"https://api.runsheet.dev/npm/..%2fmcp":                   "",
		"http://api.runsheet.dev/npm/x":                           "",
		"https://api.runsheet.dev:8443/npm/x":                     "",
		"https://registry.npmjs.org/npm/x":                        "",
		"https://files.example:8443/pkg":                          "https://files.example:8443/pkg",
		"https://files.example:8443/pkg/a":                        "https://files.example:8443/pkg",
		"https://files.example:8443/pkgx":                         "",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Match(&http.Request{URL: u}); got != want {
			t.Errorf("%s matched %q, want %q", raw, got, want)
		}
	}
	if !r.Terminates(registryHost) || r.Terminates("registry.npmjs.org") {
		t.Fatal("the rule terminates the wrong hosts")
	}
	for _, bad := range []string{"http://api.runsheet.dev/npm/", "https://user@api.runsheet.dev/npm/", "https://api.runsheet.dev/npm/?q=1", "/npm/"} {
		if _, err := NewSignRule([]string{bad}, nil, nil); err == nil {
			t.Errorf("prefix %q was taken", bad)
		}
	}
}

// claims are a proof's claims, read without checking it.
func claims(t *testing.T, proof string) dpop.Claims {
	t.Helper()
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("proof %q", proof)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c dpop.Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// A request under a prefix goes on the session token in the DPoP scheme
// with a fresh proof of exactly it — whatever Authorization or DPoP the
// session sent — and one outside goes on as sent.
func TestASignedRequestReplacesTheSessionsAuthorization(t *testing.T) {
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewSignRule(registryPrefixes, func(context.Context) (string, error) { return "tok-1", nil }, key)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "https://api.runsheet.dev/npm/@anythink%2fshell?write=true", nil)
	req.Header.Set("Authorization", "Bearer npm_from_the_sandbox")
	req.Header.Set("DPoP", "forged")
	ap, err := r.Apply(req)
	if err != nil || ap == nil || ap.Rule != RuleRegistries || ap.Ref != "https://api.runsheet.dev/npm/" || ap.Form != "dpop" {
		t.Fatalf("applied %+v %v", ap, err)
	}
	if got := req.Header.Values("Authorization"); len(got) != 1 || got[0] != "DPoP tok-1" {
		t.Fatalf("Authorization %q", got)
	}
	proof := req.Header.Get("DPoP")
	c := claims(t, proof)
	if c.HTM != "PUT" || c.HTU != "https://api.runsheet.dev/npm/@anythink%2fshell" || c.Ath != dpop.Ath("tok-1") || c.JTI == "" {
		t.Fatalf("the proof's claims %+v", c)
	}
	var v dpoptest.Verifier
	if err := v.Verify(proof, dpoptest.Request{Method: "PUT", URL: req.URL.String(), Token: "tok-1", JKT: key.Thumbprint()}); err != nil {
		t.Fatalf("the proof does not verify: %v", err)
	}

	mcp := httptest.NewRequest(http.MethodPost, "https://api.runsheet.dev/mcp", nil)
	mcp.Header.Set("Authorization", "Bearer mine")
	if ap, err := r.Apply(mcp); ap != nil || err != nil || mcp.Header.Get("Authorization") != "Bearer mine" || mcp.Header.Get("DPoP") != "" {
		t.Fatalf("a request outside the prefixes was changed: %+v %v %v", ap, err, mcp.Header)
	}

	// no token: refused, nothing the session sent goes on
	none, _ := NewSignRule(registryPrefixes, func(context.Context) (string, error) { return "", errors.New("latchkey down") }, key)
	req = httptest.NewRequest(http.MethodGet, "https://api.runsheet.dev/v2/", nil)
	_, err = none.Apply(req)
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Status != http.StatusBadGateway || ref.Code != "credential_unavailable" {
		t.Fatalf("a request without a token: %v", err)
	}
}

// registryRig is a tunnel terminating api.runsheet.dev for its
// registries, dialling every host at one fake registry with TLS, and a
// client that reaches it only through the tunnel, trusting the lease's
// CA and nothing else.
type registryRig struct {
	t        *Tunnel
	ca       *CA
	upstream *httptest.Server
	client   *http.Client
	sock     string

	mu       sync.Mutex
	requests []Request
	tokens   []string
}

func newRegistryRig(t *testing.T, handler http.HandlerFunc, token func(context.Context) (string, error)) *registryRig {
	t.Helper()
	g := &registryRig{}
	g.upstream = httptest.NewTLSServer(handler)
	t.Cleanup(g.upstream.Close)
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	rule, err := NewSignRule(registryPrefixes, token, key)
	if err != nil {
		t.Fatal(err)
	}
	if g.ca, err = NewCA("lse_test", rule.Hosts(), 0); err != nil {
		t.Fatal(err)
	}
	// the fake registry's own certificate, which the node verifies
	up := g.upstream.Client().Transport.(*http.Transport).Clone()
	up.TLSClientConfig.ServerName = "example.com"
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, g.upstream.Listener.Addr().String())
	}
	up.DialContext = dial
	up.DisableCompression = true
	g.t = &Tunnel{
		Policy: NewPolicy([]string{registryHost, "proxy.golang.org"}, nil, nil),
		Dial:   dial, Logf: t.Logf, CA: g.ca, Rules: []Rule{rule}, Upstream: up,
		Request: func(r Request) {
			g.mu.Lock()
			g.requests = append(g.requests, r)
			g.mu.Unlock()
		},
	}
	g.sock = filepath.Join(sockettest.Dir(t, "eg"), "e.sock")
	if err := g.t.Listen(g.sock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.t.Close() })
	pool := x509.NewCertPool()
	pool.AddCert(g.ca.Certificate())
	g.client = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "relay"}),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", g.sock)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	return g
}

func (g *registryRig) seen() []Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Request(nil), g.requests...)
}

// Through the tunnel a client trusting the lease's CA reaches the
// registry over TLS the relay terminates: a request under a prefix
// arrives signed with the token of the moment (a refreshed one from the
// next request on), one outside as the client sent it, on one kept-alive
// connection; each is audited by prefix, method, status and bytes.
func TestTheTunnelSignsRegistryRequestsAndForwardsTheRestAsSent(t *testing.T) {
	type seen struct{ path, auth, dpop string }
	var mu sync.Mutex
	var got []seen
	var n atomic.Int32
	g := newRegistryRig(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, seen{r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("DPoP")})
		mu.Unlock()
		io.WriteString(w, "ok "+r.URL.EscapedPath())
	}, func(context.Context) (string, error) { return fmt.Sprintf("tok-%d", n.Add(1)), nil })

	var reused []bool
	do := func(method, raw, auth string) string {
		req, _ := http.NewRequest(method, raw, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(i httptrace.GotConnInfo) { reused = append(reused, i.Reused) },
		}))
		resp, err := g.client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, raw, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.TLS == nil || resp.TLS.PeerCertificates[0].Issuer.CommonName != g.ca.Certificate().Subject.CommonName {
			t.Fatalf("%s was not answered by the relay's certificate", raw)
		}
		return string(b)
	}
	if b := do("GET", "https://api.runsheet.dev/npm/@anythink%2fshell", "Bearer from-npmrc"); b != "ok /npm/@anythink%2fshell" {
		t.Fatalf("answered %q", b)
	}
	do("GET", "https://api.runsheet.dev/mcp", "Bearer mine")
	do("GET", "https://api.runsheet.dev/npmx/left-pad", "")
	do("HEAD", "https://api.runsheet.dev/v2/acme/web/app/manifests/x", "")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("the registry saw %+v", got)
	}
	if got[0].auth != "DPoP tok-1" || claims(t, got[0].dpop).HTU != "https://api.runsheet.dev/npm/@anythink%2fshell" {
		t.Fatalf("the npm request arrived %+v", got[0])
	}
	if got[1].auth != "Bearer mine" || got[1].dpop != "" || got[2].auth != "" || got[2].dpop != "" {
		t.Fatalf("a request outside the prefixes was changed: %+v", got[1:3])
	}
	if got[3].auth != "DPoP tok-2" || claims(t, got[3].dpop).HTM != "HEAD" {
		t.Fatalf("the refreshed token was not used: %+v", got[3])
	}
	if len(reused) != 4 || reused[0] || !reused[1] || !reused[2] || !reused[3] {
		t.Fatalf("the connection was not kept alive: %v", reused)
	}
	rs := g.seen()
	if len(rs) != 4 || len(rs[0].Applied) != 1 || rs[0].Applied[0].Ref != "https://api.runsheet.dev/npm/" || rs[0].Status != 200 || rs[0].Bytes == 0 ||
		rs[0].Method != "GET" || rs[0].Host != registryHost || len(rs[1].Applied) != 0 || len(rs[3].Applied) != 1 {
		t.Fatalf("the audit %+v", rs)
	}
}

// Bodies stream both ways: the registry has the first bytes of an upload
// while the client is still sending it, and the client has the first
// bytes of an answer while the registry is still writing it.
func TestTheTunnelStreamsBodiesUnbuffered(t *testing.T) {
	gotFirst := make(chan struct{})
	release := make(chan struct{})
	var received atomic.Int64
	g := newRegistryRig(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			buf := make([]byte, 4)
			if _, err := io.ReadFull(r.Body, buf); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			close(gotFirst)
			n, _ := io.Copy(io.Discard, r.Body)
			received.Store(n + 4)
			w.WriteHeader(http.StatusCreated)
			return
		}
		io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, strings.Repeat("x", 1<<20))
	}, func(context.Context) (string, error) { return "tok", nil })

	// upload: the rest of the body is sent only once the registry has
	// its first bytes
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPut, "https://api.runsheet.dev/npm/@anythink%2fshell", pr)
		resp, err := g.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				err = fmt.Errorf("status %d", resp.StatusCode)
			}
		}
		done <- err
	}()
	pw.Write([]byte("head"))
	select {
	case <-gotFirst:
	case <-time.After(10 * time.Second):
		t.Fatal("the registry had nothing of the upload while it was being sent: the body was buffered")
	}
	chunk := bytes.Repeat([]byte("y"), 64<<10)
	for range 32 {
		pw.Write(chunk)
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if received.Load() != 4+32*64<<10 {
		t.Fatalf("the registry received %d bytes", received.Load())
	}

	// download: the first bytes arrive before the registry writes the rest
	resp, err := g.client.Get("https://api.runsheet.dev/v2/acme/web/blobs/sha256:1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, 5)
	read := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, first); read <- err }()
	select {
	case err := <-read:
		if err != nil || string(first) != "first" {
			t.Fatalf("first bytes %q %v", first, err)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the answer's first bytes did not arrive before its end: the body was buffered")
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if len(rest) != 1<<20 {
		t.Fatalf("the rest was %d bytes", len(rest))
	}
}

// A host no rule names is spliced as before: the client sees the host's
// own certificate, not one the relay minted. So is a host a rule names
// when the CONNECT is a relay sidecar's own (SpliceOnly).
func TestHostsNoRuleNamesAndTheSidecarsOwnAreSpliced(t *testing.T) {
	g := newRegistryRig(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "auth="+r.Header.Get("Authorization"))
	}, func(context.Context) (string, error) { return "tok", nil })
	insecure := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "relay"}),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", g.sock)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	upstreamCert := g.upstream.Certificate()
	get := func(c *http.Client, raw string) (string, *x509.Certificate) {
		resp, err := c.Get(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.TLS.PeerCertificates[0]
	}
	if body, cert := get(insecure, "https://proxy.golang.org/x"); !cert.Equal(upstreamCert) || body != "auth=" {
		t.Fatalf("an unnamed host was not spliced: %q, cert issued by %s", body, cert.Issuer)
	}
	if len(g.seen()) != 0 {
		t.Fatalf("a spliced request was audited as terminated: %+v", g.seen())
	}

	// the sidecar's own connection to the registry's host
	sock := filepath.Join(sockettest.Dir(t, "sp"), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.t.ServeHTTP(w, r.WithContext(SpliceOnly(r.Context())))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	own := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "relay"}),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	if body, cert := get(own, "https://api.runsheet.dev/npm/x"); !cert.Equal(upstreamCert) || body != "auth=" {
		t.Fatalf("the sidecar's own connection was terminated: %q, cert issued by %s", body, cert.Issuer)
	}
}

// A client that does not trust the lease's CA gets no answer from a
// terminated host, and the registry sees nothing.
func TestAClientThatDoesNotTrustTheCAIsNotAnswered(t *testing.T) {
	var hits atomic.Int32
	g := newRegistryRig(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) },
		func(context.Context) (string, error) { return "tok", nil })
	g.client.Transport.(*http.Transport).TLSClientConfig.RootCAs = x509.NewCertPool()
	if _, err := g.client.Get("https://api.runsheet.dev/npm/x"); err == nil {
		t.Fatal("a client that trusts no relay CA was answered")
	}
	if hits.Load() != 0 {
		t.Fatal("the registry was reached")
	}
}

// curl, as npm or crane would, fetches and uploads through the tunnel
// on the lease's CA alone (--cacert), sending no credential: the
// registry gets every byte, each request signed.
func TestCurlReachesTheRegistryThroughTheTunnel(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	var mu sync.Mutex
	var auths []string
	var uploaded atomic.Int64
	g := newRegistryRig(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Method == http.MethodPut {
			n, _ := io.Copy(io.Discard, r.Body)
			uploaded.Store(n)
			w.WriteHeader(http.StatusCreated)
			return
		}
		io.WriteString(w, `{"name":"@anythink/shell"}`)
	}, func(context.Context) (string, error) { return "tok", nil })
	// curl reaches the tunnel by TCP, as a pod's HTTPS_PROXY does
	front := httptest.NewServer(g.t)
	defer front.Close()
	dir := t.TempDir()
	caFile, tarball := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "pkg.tgz")
	os.WriteFile(caFile, g.ca.CertPEM(), 0o600)
	os.WriteFile(tarball, bytes.Repeat([]byte("z"), 8<<20), 0o600)
	run := func(args ...string) string {
		cmd := exec.Command(curl, append([]string{"-sS", "--fail", "--proxy", front.URL, "--cacert", caFile}, args...)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("curl %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	if out := run("https://api.runsheet.dev/npm/@anythink%2fshell"); out != `{"name":"@anythink/shell"}` {
		t.Fatalf("curl got %q", out)
	}
	run("-T", tarball, "https://api.runsheet.dev/npm/@anythink%2fshell")
	if uploaded.Load() != 8<<20 {
		t.Fatalf("the registry got %d bytes of the upload", uploaded.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 || auths[0] != "DPoP tok" || auths[1] != "DPoP tok" {
		t.Fatalf("the registry saw %q", auths)
	}
	// the CA's PEM is a certificate a client can load
	if b, _ := pem.Decode(g.ca.CertPEM()); b == nil || b.Type != "CERTIFICATE" {
		t.Fatal("the CA's PEM")
	}
}
