package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sockettest"
)

// rig is a tunnel on a socket of its own whose Dial reaches every
// allowed host at one TLS test server, and a client that reaches the
// internet only through it, as a pod's HTTPS_PROXY does.
type rig struct {
	t      *Tunnel
	sock   string
	client *http.Client

	mu    sync.Mutex
	calls []Call
}

func newRig(t *testing.T, p *Policy) *rig {
	t.Helper()
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "reached "+r.Host)
	}))
	t.Cleanup(host.Close)
	g := &rig{}
	g.t = &Tunnel{Policy: p, Logf: t.Logf,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, host.Listener.Addr().String())
		},
		Audit: func(c Call) {
			g.mu.Lock()
			g.calls = append(g.calls, c)
			g.mu.Unlock()
		},
	}
	g.sock = filepath.Join(sockettest.Dir(t, "eg"), "e.sock")
	if err := g.t.Listen(g.sock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.t.Close() })
	g.client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "relay"}),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", g.sock)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	return g
}

func (g *rig) get(rawURL string) (string, error) {
	resp, err := g.client.Get(rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func (g *rig) audited() []Call {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Call(nil), g.calls...)
}

// A session of a kind with no hosts is refused a package registry, and
// one whose kind names a host reaches it, and only it, by HTTPS.
func TestTunnelRefusesARegistryTheKindDoesNotNameAndReachesOneItDoes(t *testing.T) {
	none := newRig(t, NewPolicy([]string{}, nil, nil))
	for _, u := range []string{"https://registry.npmjs.org/left-pad", "https://proxy.golang.org/golang.org/x/text/@v/list"} {
		if body, err := none.get(u); err == nil || !strings.Contains(err.Error(), "Forbidden") {
			t.Fatalf("a kind with no hosts reached %s: %q %v", u, body, err)
		}
	}
	if c := none.audited(); len(c) != 2 || c[0].Allowed || c[0].Host != "registry.npmjs.org" || c[0].Port != "443" || !strings.Contains(c[0].Refused, "not in the kind's egress") {
		t.Fatalf("audit %+v", c)
	}

	some := newRig(t, NewPolicy([]string{"proxy.golang.org"}, nil, nil))
	body, err := some.get("https://proxy.golang.org/golang.org/x/text/@v/list")
	if err != nil || body != "reached proxy.golang.org" {
		t.Fatalf("an allowed host answered %q %v", body, err)
	}
	if _, err := some.get("https://registry.npmjs.org/left-pad"); err == nil {
		t.Fatal("a host the kind does not name was reached")
	}
	if _, err := some.get("https://PROXY.golang.org:8443/x"); err == nil {
		t.Fatal("an allowed host on another port was reached")
	}
	resp, err := some.client.Get("http://proxy.golang.org/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get(RefusedHeader) != "not_https" {
		t.Fatalf("plain HTTP through the tunnel answered %d %v", resp.StatusCode, resp.Header)
	}
	if c := some.audited(); len(c) != 4 || !c[0].Allowed || c[0].Host != "proxy.golang.org" || c[2].Refused == "" {
		t.Fatalf("audit %+v", c)
	}
}

// A tunnel to an allowed host whose TLS server name is another host is
// closed before anything is sent on: the SNI must be the host asked for.
func TestTunnelRefusesAServerNameThatIsAnotherHost(t *testing.T) {
	g := newRig(t, NewPolicy([]string{"proxy.golang.org"}, nil, nil))
	conn, err := net.Dial("unix", g.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "CONNECT proxy.golang.org:443 HTTP/1.1\r\nHost: proxy.golang.org:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT answered %+v %v", resp, err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	tc := tls.Client(conn, &tls.Config{ServerName: "evil.example", InsecureSkipVerify: true})
	if err := tc.Handshake(); err == nil {
		t.Fatal("a handshake for another server name went through")
	}
	if c := g.audited(); len(c) != 1 || c[0].Allowed || !strings.Contains(c[0].Refused, "evil.example") {
		t.Fatalf("audit %+v", c)
	}
}

// The tenant's policy narrows the kind's hosts and never widens them;
// fixed upstreams stay whatever the tenant says.
func TestPolicyIsTheKindsHostsNarrowedByTheTenantPlusFixed(t *testing.T) {
	p := NewPolicy([]string{"proxy.golang.org", "Registry.npmjs.org."}, []string{"registry.npmjs.org", "pypi.org"}, []string{"api.anthropic.com"})
	got := p.Hosts()
	sort.Strings(got)
	if want := []string{"api.anthropic.com", "registry.npmjs.org"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts %v, want %v", got, want)
	}
	if ok, _, _ := p.Allow("REGISTRY.npmjs.org", "443"); !ok {
		t.Error("an allowed host in upper case was refused")
	}
	if ok, code, _ := p.Allow("registry.npmjs.org", "80"); ok || code != "port" {
		t.Error("port 80 was allowed")
	}
	if ok, _, _ := p.Allow("pypi.org", "443"); ok {
		t.Error("the tenant widened the kind's egress")
	}
	if ok, _, _ := NewPolicy(nil, nil, nil).Allow("example.com", "443"); ok {
		t.Error("the empty policy allowed a host")
	}
	if ok, _, _ := (*Policy)(nil).Allow("example.com", "443"); ok {
		t.Error("a nil policy allowed a host")
	}
}

// publicDial never reaches a private, loopback or link-local address.
func TestPublicIPRefusesPrivateRanges(t *testing.T) {
	for _, a := range []string{"10.0.0.1", "192.168.1.1", "172.16.0.1", "127.0.0.1", "169.254.1.1", "100.64.0.1", "::1", "fe80::1", "fd00::1", "0.0.0.0"} {
		if publicIP(net.ParseIP(a)) {
			t.Errorf("%s is taken as public", a)
		}
	}
	for _, a := range []string{"8.8.8.8", "2606:4700::1111"} {
		if !publicIP(net.ParseIP(a)) {
			t.Errorf("%s is not taken as public", a)
		}
	}
}
