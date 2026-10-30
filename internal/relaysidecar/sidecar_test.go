package relaysidecar

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
	"projectmesh.io/mesh-cli/internal/sockettest"
)

// testTunnel stands in for the node's egress tunnel (internal/egress):
// a CONNECT to a host:port it names is piped to that host's test
// server, anything else refused, and every ask kept.
type testTunnel struct {
	routes map[string]string // host:port asked → the test server's address

	mu    sync.Mutex
	asked []string
}

func (tt *testTunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tt.mu.Lock()
	tt.asked = append(tt.asked, r.Host)
	tt.mu.Unlock()
	addr, ok := tt.routes[r.Host]
	if !ok {
		http.Error(w, r.Host+" is not on the lease's egress", http.StatusForbidden)
		return
	}
	up, err := net.Dial("tcp", addr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	down, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		up.Close()
		return
	}
	_, _ = io.WriteString(down, "HTTP/1.1 200 Connection Established\r\n\r\n")
	pipe(down, up)
}

// rig is a lease's node side and its sidecar, in process: the sidecar
// on a loopback port as the pod's loopback, the node on a unix socket,
// and TLS upstreams reached only through the test tunnel.
type rig struct {
	t       *testing.T
	sidecar *Sidecar
	base    string // the sidecar's loopback root
	dir     string // the lease's socket directory
	tunnel  *testTunnel
	roots   *x509.CertPool

	mu       sync.Mutex
	access   []meshapi.AccessRecord
	security chan meshapi.SecurityReport
	renewals int
	renewed  []RenewRequest
	auth     []string // the product's Authorization headers
}

func newRig(t *testing.T, creds Credentials, upstreams map[string]http.Handler) *rig {
	t.Helper()
	g := &rig{t: t, dir: sockettest.Dir(t, "rs"), tunnel: &testTunnel{routes: map[string]string{}}, roots: x509.NewCertPool(),
		security: make(chan meshapi.SecurityReport, 16)}
	product := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		g.mu.Unlock()
		if r.Header.Get("DPoP") == "" {
			http.Error(w, "no proof", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"from":"product"}}`)
	}))
	t.Cleanup(product.Close)
	g.route("product.example.com:443", product)
	for host, h := range upstreams {
		srv := httptest.NewTLSServer(h)
		t.Cleanup(srv.Close)
		g.route(host, srv)
	}
	tools := &mcptool.Handler{Name: DefaultNodeToolsServer, Tools: []mcptool.Tool{{
		Name: "progress", Description: "say how it goes",
		Schema: map[string]any{"type": "object"},
		Call:   func(context.Context, map[string]any) (string, error) { return "noted", nil },
	}}}
	nodeProxy := sessionproxy.New(map[string]sessionproxy.Upstream{DefaultNodeToolsServer: sessionproxy.InProcess{Handler: tools}})
	node := &Node{
		Tunnel: g.tunnel,
		Proxy:  nodeProxy,
		Stop: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"decision":"block","reason":"finish first"}`)
		}),
		Renew: func(_ context.Context, req RenewRequest) (Renewal, error) {
			g.mu.Lock()
			g.renewals++
			g.renewed = append(g.renewed, req)
			g.mu.Unlock()
			out := Renewal{Session: Token{Value: "renewed-session", ExpiresAt: time.Now().Add(10 * time.Minute)}}
			if req.Git {
				out.Git = &Token{Value: "renewed-git", ExpiresAt: time.Now().Add(time.Hour)}
				if req.Repo != "" {
					out.Git.Value = "read-" + req.Repo
				}
			}
			return out, nil
		},
		Access: func(rec meshapi.AccessRecord) {
			g.mu.Lock()
			g.access = append(g.access, rec)
			g.mu.Unlock()
		},
		Security: func(rep meshapi.SecurityReport) { g.security <- rep },
		Logf:     t.Logf,
	}
	nodeSock := filepath.Join(g.dir, "n")
	if err := node.Listen(nodeSock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { node.Close() })
	s, err := New(creds, nodeSock)
	if err != nil {
		t.Fatal(err)
	}
	s.Logf = t.Logf
	tr := s.upstream.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{RootCAs: g.roots}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	if err := s.ServeControl(filepath.Join(g.dir, "r")); err != nil {
		t.Fatal(err)
	}
	g.sidecar, g.base = s, "http://"+ln.Addr().String()
	return g
}

// route has the tunnel reach host (host:port as asked) at srv, whose
// certificate the sidecar trusts.
func (g *rig) route(host string, srv *httptest.Server) {
	g.tunnel.routes[host] = srv.Listener.Addr().String()
	g.roots.AddCert(srv.Certificate())
}

func (g *rig) post(path, body string) (int, string) {
	g.t.Helper()
	resp, err := http.Post(g.base+path, "application/json", strings.NewReader(body))
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (g *rig) records() []meshapi.AccessRecord {
	g.sidecar.Flush()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.mu.Lock()
		n := len(g.access)
		g.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]meshapi.AccessRecord(nil), g.access...)
}

// The product's MCP server is reached through the node's tunnel on the
// lease's session token, DPoP-bound, signed in the sidecar; the node
// tools and the Stop hook reach the node unchanged; the node's own
// routes are not the session's to call.
func TestSidecarServesTheSessionsRoutes(t *testing.T) {
	c := testCreds(t)
	c.Servers = []Server{{Name: "runsheet", URL: "https://product.example.com/mcp"}}
	g := newRig(t, c, nil)

	code, body := g.post("/mcp/runsheet", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if code != 200 || !strings.Contains(body, "product") {
		t.Fatalf("the product route answered %d %s", code, body)
	}
	g.mu.Lock()
	auth := append([]string(nil), g.auth...)
	g.mu.Unlock()
	if len(auth) != 1 || auth[0] != "DPoP "+sessionSecret {
		t.Fatalf("the product saw Authorization %q, want the lease's DPoP session token", auth)
	}

	code, body = g.post("/mcp/"+DefaultNodeToolsServer, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if code != 200 || !strings.Contains(body, `"progress"`) {
		t.Fatalf("the node tools answered %d %s", code, body)
	}
	code, body = g.post(StopPath, `{}`)
	if code != 200 || !strings.Contains(body, "finish first") {
		t.Fatalf("the Stop hook answered %d %s", code, body)
	}
	for _, p := range []string{RenewPath, AccessPath, PushPath} {
		if code, _ := g.post(p, `{}`); code != http.StatusNotFound {
			t.Errorf("%s on the session's loopback answered %d, want 404", p, code)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.renewals != 0 {
		t.Fatalf("a live session token was renewed %d times", g.renewals)
	}
}

// A session token near its end is renewed through the node, and the
// product sees the renewed one.
func TestSidecarRenewsThroughTheNode(t *testing.T) {
	c := testCreds(t)
	c.Servers = []Server{{Name: "runsheet", URL: "https://product.example.com/mcp"}}
	c.Session.ExpiresAt = time.Now().Add(10 * time.Second)
	g := newRig(t, c, nil)
	if code, body := g.post("/mcp/runsheet", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); code != 200 {
		t.Fatalf("the product route answered %d %s", code, body)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.renewals != 1 || len(g.auth) != 1 || g.auth[0] != "DPoP renewed-session" {
		t.Fatalf("renewals %d, product saw %q; want one renewal and the renewed token", g.renewals, g.auth)
	}
}

// The session's HTTPS_PROXY: a host on the lease's egress is reached
// through the node's tunnel, TLS end to end from the session to the
// host; one that is not is refused 403 with the node's reason and never
// dialled.
func TestSidecarEgressGoesThroughTheTunnel(t *testing.T) {
	allowed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "reached "+r.Host) })
	g := newRig(t, testCreds(t), map[string]http.Handler{"allowed.example.com:443": allowed})
	proxyURL, _ := url.Parse(g.base)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: g.roots},
	}}
	resp, err := client.Get("https://allowed.example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "reached allowed.example.com" {
		t.Fatalf("the allowed host answered %d %q", resp.StatusCode, b)
	}
	_, err = client.Get("https://denied.example.com/x")
	if err == nil {
		t.Fatal("a host off the lease's egress was reached")
	}
	// the refusal itself, as a CONNECT sees it
	c, err := net.Dial("tcp", strings.TrimPrefix(g.base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "CONNECT denied.example.com:443 HTTP/1.1\r\nHost: denied.example.com:443\r\n\r\n")
	r, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	why, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusForbidden || !strings.Contains(string(why), "not on the lease's egress") {
		t.Fatalf("a refused CONNECT answered %d %q", r.StatusCode, why)
	}
}

// What the sidecar sends the node is AccessRecords of the kinds it
// keeps; the node drops any other.
func TestNodeKeepsTheSidecarsAccessRecords(t *testing.T) {
	var kept []meshapi.AccessRecord
	n := &Node{Access: func(r meshapi.AccessRecord) { kept = append(kept, r) }}
	body, _ := json.Marshal([]meshapi.AccessRecord{{Kind: "git", Repo: "acme/app"}, {Kind: "machine"}, {Kind: "api", Vendor: "github"}})
	rec := httptest.NewRecorder()
	n.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, AccessPath, strings.NewReader(string(body))))
	if rec.Code != http.StatusNoContent || len(kept) != 2 {
		t.Fatalf("answered %d, kept %+v", rec.Code, kept)
	}
	rec = httptest.NewRecorder()
	n.ServeHTTP(rec, httptest.NewRequest(http.MethodConnect, "http://x/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a CONNECT with no tunnel answered %d", rec.Code)
	}
}

// The sidecar's security reports reach the node, which keeps only the
// content-free ones: a report naming a URL as its host, or a rule that
// is not a name, goes no further.
func TestNodeKeepsOnlyContentFreeSecurityReports(t *testing.T) {
	var kept []meshapi.SecurityReport
	n := &Node{Security: func(r meshapi.SecurityReport) { kept = append(kept, r) }}
	body, _ := json.Marshal([]meshapi.SecurityReport{
		{Type: "egress", Host: "plain.example", Verdict: "deny", Rule: "egress.none"},
		{Type: "egress", Host: "http://plain.example/steal?token=abc", Verdict: "deny"},
		{Type: "sandbox", Verdict: "deny", Rule: "Authorization: Bearer x"},
	})
	rec := httptest.NewRecorder()
	n.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, SecurityPath, strings.NewReader(string(body))))
	if rec.Code != http.StatusNoContent || len(kept) != 1 || kept[0].Host != "plain.example" {
		t.Fatalf("answered %d, kept %+v", rec.Code, kept)
	}
}

// A plain-HTTP request the sidecar refuses itself is passed to the node
// as a report by host and rule alone.
func TestSidecarPassesItsOwnRefusalsToTheNode(t *testing.T) {
	g := newRig(t, testCreds(t), nil)
	got := g.security
	conn, err := net.Dial("tcp", strings.TrimPrefix(g.base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET http://plain.example/steal?token=abc HTTP/1.1\r\nHost: plain.example\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a plain-HTTP request answered %d", resp.StatusCode)
	}
	select {
	case r := <-got:
		if r.Type != "egress" || r.Host != "plain.example" || r.Verdict != "deny" || r.Rule != "egress.none" || r.Bytes != 0 {
			t.Fatalf("the report %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the node heard no report")
	}
}

// A read repo's fetch goes upstream through the tunnel on a read token
// the node mints for that repository when the sidecar first asks
// (RenewRequest.Repo), cached for the next; the held token of the job's
// own repository is untouched, and a push to the read repo is refused
// without asking the node for anything.
func TestSidecarFetchesAReadRepoOnATokenTheNodeMints(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	vendor := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, _ := r.BasicAuth()
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+pass)
		mu.Unlock()
		_, _ = io.WriteString(w, "refs")
	})
	c := testCreds(t)
	c.Git.Upstream = "https://git.example.com"
	c.Git.Policy.ReadRepos = []string{"acme/lib"}
	g := newRig(t, c, map[string]http.Handler{"git.example.com:443": vendor})
	get := func(path string) int {
		t.Helper()
		resp, err := http.Get(g.base + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	for range 2 {
		if code := get("/git/acme/lib.git/info/refs?service=git-upload-pack"); code != 200 {
			t.Fatalf("the read repo's fetch answered %d", code)
		}
	}
	if code := get("/git/acme/app.git/info/refs?service=git-upload-pack"); code != 200 {
		t.Fatalf("the own repository's fetch answered %d", code)
	}
	if code := get("/git/acme/lib.git/info/refs?service=git-receive-pack"); code != http.StatusForbidden {
		t.Fatalf("a push to the read repo answered %d", code)
	}
	mu.Lock()
	want := []string{"/acme/lib.git/info/refs read-acme/lib", "/acme/lib.git/info/refs read-acme/lib", "/acme/app.git/info/refs " + gitSecret}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("the vendor saw %q, want %q", seen, want)
	}
	mu.Unlock()
	g.mu.Lock()
	renewed := append([]RenewRequest(nil), g.renewed...)
	g.mu.Unlock()
	if len(renewed) != 1 || !renewed[0].Git || renewed[0].Repo != "acme/lib" {
		t.Fatalf("renewals %+v, want one for acme/lib", renewed)
	}
	var lib int
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		lib = 0
		for _, rec := range g.records() {
			if rec.Kind == "git" && rec.Repo == "acme/lib" {
				lib++
			}
		}
		if lib >= 3 || time.Now().After(deadline) {
			break
		}
	}
	if lib != 3 {
		t.Fatalf("%d access records name acme/lib, want 3", lib)
	}
}

// A filled tool slot (ADR 001 amendment, slot bindings) is served at
// /mcp/<slot> through the node's tunnel to Latchkey's proxy for its
// binding, on the lease's DPoP session token, held to its one tool under
// the kind's description; a slot needs a Latchkey origin.
func TestSidecarServesAFilledSlotThroughLatchkey(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	latchkey := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "tools/list") {
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"web_search","description":"vendor words"},{"name":"other","description":"x"}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"found"}]}}`)
	})
	c := testCreds(t)
	c.Slots = []Slot{{Name: "web_search", Description: "Search the web.", Binding: "bind-1"}}
	c.API, c.Latchkey = nil, ""
	if err := c.Validate(); err == nil {
		t.Fatal("slots without a Latchkey origin were accepted")
	}
	c.Latchkey = "https://latchkey.example.com"
	g := newRig(t, c, map[string]http.Handler{"latchkey.example.com:443": latchkey})

	code, body := g.post("/mcp/web_search", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if code != 200 || !strings.Contains(body, `"Search the web."`) || strings.Contains(body, "other") || strings.Contains(body, "vendor words") {
		t.Fatalf("the slot's list answered %d %s", code, body)
	}
	code, body = g.post("/mcp/web_search", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"web_search","arguments":{}}}`)
	if code != 200 || !strings.Contains(body, "found") {
		t.Fatalf("the slot's call answered %d %s", code, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("Latchkey saw %q", seen)
	}
	for _, s := range seen {
		if s != "/v1/connections/for-job/bind-1/mcp DPoP "+sessionSecret {
			t.Errorf("Latchkey saw %q, want the binding's route on the session token", s)
		}
	}
}
