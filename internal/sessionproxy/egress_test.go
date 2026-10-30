package sessionproxy

import (
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
