package nodev1

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
	"projectmesh.io/mesh-cli/internal/sockettest"
)

// A tools/call response marked result._meta["mesh/untrusted"]: true
// taints the lease: a later plain MCP read still succeeds and a later
// model-route request is unaffected, while a later git push and a later
// egress dial through the lease's tunnel are both refused naming the
// taint.
func TestTaint(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// the first call answers content marked untrusted
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"evil"}],"_meta":{"mesh/untrusted":true}}}`)
			return
		}
		// every later call is a plain, unmarked answer
		io.WriteString(w, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"fine"}]}}`)
	}))
	defer up.Close()

	tnt := newTaintState("lease-1", t.Logf)

	p := sessionproxy.New(map[string]sessionproxy.Upstream{"prod": sessionproxy.Remote{URL: up.URL}})
	p.Observe = observeBoth(nil, tnt.observe)
	p.Model = &sessionproxy.Model{
		Upstreams: map[string]string{"anthropic": up.URL},
		Credential: func(context.Context, string, bool) (sessionproxy.ModelCredential, error) {
			return sessionproxy.ModelCredential{Header: "Authorization", Value: "Bearer real"}, nil
		},
	}

	dir := sockettest.Dir(t, "tnt")
	sock := filepath.Join(dir, "mcp.sock")
	if err := p.Listen(sock); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	mcpClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}

	modelBase, err := p.ListenModel()
	if err != nil {
		t.Fatal(err)
	}

	if tnt.Tainted() {
		t.Fatal("tainted before any call")
	}

	// the tools/call answering untrusted content taints the lease
	resp, err := mcpClient.Post("http://proxy"+sessionproxy.Prefix+"prod", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fetch"}}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if !tnt.Tainted() {
		t.Fatal("a tools/call answering untrusted content did not taint the lease")
	}
	if !strings.Contains(tnt.reasonFor(), "prod") || !strings.Contains(tnt.reasonFor(), "fetch") {
		t.Fatalf("taint reason %q names neither the server nor the tool", tnt.reasonFor())
	}

	// a later plain MCP read still succeeds
	resp, err = mcpClient.Post("http://proxy"+sessionproxy.Prefix+"prod", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"other"}}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "fine") {
		t.Fatalf("a plain MCP read after taint answered %d %q, want it to still succeed", resp.StatusCode, body)
	}

	// a later model-route request is unaffected
	mresp, err := http.Get(modelBase + "/model/anthropic/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	mbody, _ := io.ReadAll(mresp.Body)
	mresp.Body.Close()
	if mresp.StatusCode != http.StatusOK || !strings.Contains(string(mbody), "fine") {
		t.Fatalf("a model-route request after taint answered %d %q, want it unaffected", mresp.StatusCode, mbody)
	}

	// a later git push is refused naming the taint, never reaching Grant
	// or Pusher
	push := &nodetools.Push{
		Pusher: func(ctx context.Context, ref, sha, lease string) (nodetools.PushOutcome, error) {
			t.Fatal("Pusher called while the lease is tainted")
			return nodetools.PushOutcome{}, nil
		},
		Grant:     fatalGrantTaint{t},
		Repo:      "acme/fixture",
		Workspace: t.TempDir(),
		PushTo:    "main",
		Tainted:   tnt.Tainted,
	}
	if _, err := push.Tool().Call(context.Background(), map[string]any{"ref": "main"}); err == nil || !strings.Contains(err.Error(), "tainted") {
		t.Fatalf("a push after taint answered %v, want a refusal naming the taint", err)
	}

	// a later egress dial through the lease's tunnel is refused naming
	// the taint
	egHost := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "reached "+r.Host)
	}))
	defer egHost.Close()
	tunnel := &egress.Tunnel{
		Policy: egress.NewPolicy([]string{"example.com"}, nil, nil),
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, egHost.Listener.Addr().String())
		},
		Tainted: tnt.Tainted,
	}
	tunnelSock := filepath.Join(sockettest.Dir(t, "tnteg"), "e.sock")
	if err := tunnel.Listen(tunnelSock); err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()
	egClient := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "relay"}),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", tunnelSock)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	eresp, err := egClient.Get("https://example.com/x")
	if err == nil {
		eresp.Body.Close()
		t.Fatal("a tainted tunnel reached its allowed host")
	}
	if !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("a tainted tunnel's dial answered %v, want Forbidden", err)
	}
}

// fatalGrantTaint fails the test if it is ever asked for its base: a
// tainted push must never touch Grant.
type fatalGrantTaint struct{ t *testing.T }

func (g fatalGrantTaint) Base() string {
	g.t.Fatal("Grant.Base called while the lease is tainted")
	return ""
}
