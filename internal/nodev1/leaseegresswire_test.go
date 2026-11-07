package nodev1

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
	"projectmesh.io/mesh-cli/internal/sockettest"
)

const leaseEgressTestHost = "registry.example.test"

// leaseEgressRig runs mesh lease-egress's own pieces in process exactly
// as leaseegresscmd.go wires them: a tunnel, its LeaseEgressServer, the
// node socket a session's CONNECT lands on, and the control socket the
// node drives it over — so a test proves the wire protocol (adr-051
// decision 2), not a shortcut around it. A terminated request reaches
// its registry stand-in through Tunnel.Upstream; a spliced one (no
// termination rule matches) reaches the same stand-in through Tunnel.Dial.
type leaseEgressRig struct {
	srv      *LeaseEgressServer
	registry *httptest.Server
	nodeSock string
	ctlSock  string
	client   *http.Client
}

func newLeaseEgressRig(t *testing.T, handler http.HandlerFunc) *leaseEgressRig {
	t.Helper()
	g := &leaseEgressRig{registry: httptest.NewTLSServer(handler)}
	t.Cleanup(g.registry.Close)

	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, g.registry.Listener.Addr().String())
	}
	up := g.registry.Client().Transport.(*http.Transport).Clone()
	up.TLSClientConfig.ServerName = "example.com"
	up.DialContext = dial
	up.DisableCompression = true

	tunnel := &egress.Tunnel{Logf: t.Logf, Dial: dial, Upstream: up}
	g.srv = NewLeaseEgressServer(tunnel, t.Logf)

	dir := sockettest.Dir(t, "eg")
	g.nodeSock = filepath.Join(dir, "node.sock")
	g.ctlSock = filepath.Join(dir, "ctl.sock")

	nodeClose, err := ServeTunnelSocket(tunnel, g.nodeSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nodeClose.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = g.srv.ServeControl(ctx, g.ctlSock)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	g.client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "relay"}),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", g.nodeSock)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		// Every request is its own CONNECT and its own audited call: a
		// kept-alive connection would reuse one tunnel for many requests,
		// which both tests' event counts assume never happens.
		DisableKeepAlives: true,
	}}
	return g
}

// control is the node's own client of the control socket, returned only
// once dialling it succeeds: ServeControl starts in its own goroutine
// and binds the socket with no signal back to the caller.
func (g *leaseEgressRig) control(t *testing.T) LeaseEgressClient {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("unix", g.ctlSock)
		if err == nil {
			conn.Close()
			return LeaseEgressClient{Socket: g.ctlSock}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the control socket never came up: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A test starts mesh lease-egress's pieces, pushes a session token, a
// DPoP key and a sign rule over the control socket, then makes a CONNECT
// request under the signed prefix through the node socket: the upstream
// request carries a DPoP proof signed by the pushed key with the pushed
// token, and the audit call for it arrives on the control connection
// with the fields egress.Tunnel.Audit reports today.
func TestLeaseEgressSignsAndAuditsAConnectThroughTheControlSocket(t *testing.T) {
	var mu sync.Mutex
	var gotAuth, gotDPoP string
	g := newLeaseEgressRig(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth, gotDPoP = r.Header.Get("Authorization"), r.Header.Get("DPoP")
		mu.Unlock()
		io.WriteString(w, "reached "+r.URL.Path)
	})
	ctx := context.Background()
	ctl := g.control(t)

	prefix := fmt.Sprintf("https://%s/v2/", leaseEgressTestHost)
	startRes, err := ctl.Start(ctx, LeaseEgressStartRequest{
		Hosts: []string{leaseEgressTestHost},
		Rules: []egress.RuleSpec{{Kind: "sign", Registries: []string{prefix}}},
	})
	if err != nil || !startRes.Started {
		t.Fatalf("start: %+v, %v", startRes, err)
	}

	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := key.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	const pushedToken = "mesh-session-token-1"
	credRes, err := ctl.Credentials(ctx, LeaseEgressCredentialsRequest{
		Key:     handoff,
		Session: relaysidecar.Token{Value: pushedToken},
	})
	if err != nil || !credRes.Replaced {
		t.Fatalf("credentials: %+v, %v", credRes, err)
	}

	auditCtx, stopAudit := context.WithCancel(context.Background())
	defer stopAudit()
	resyncCh := make(chan AuditResync, 1)
	var auditMu sync.Mutex
	var events []AuditEvent
	go func() {
		_ = ctl.Audit(auditCtx, func(rs AuditResync) { resyncCh <- rs }, func(ev AuditEvent) {
			auditMu.Lock()
			events = append(events, ev)
			auditMu.Unlock()
		})
	}()
	select {
	case <-resyncCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no resync on the control connection")
	}

	nodeURL := prefix + "pkg"
	resp, err := g.client.Get(nodeURL)
	if err != nil {
		t.Fatalf("the request through the node socket: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "reached /v2/pkg" {
		t.Fatalf("the registry answered %q", body)
	}

	mu.Lock()
	auth, proof := gotAuth, gotDPoP
	mu.Unlock()
	if auth != "DPoP "+pushedToken {
		t.Fatalf("Authorization %q, want DPoP %s", auth, pushedToken)
	}
	var v dpoptest.Verifier
	if err := v.Verify(proof, dpoptest.Request{Method: http.MethodGet, URL: nodeURL, Token: pushedToken, JKT: key.Thumbprint()}); err != nil {
		t.Fatalf("the proof does not verify: %v", err)
	}

	var call, doneEvent *egress.Call
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		auditMu.Lock()
		for _, ev := range events {
			if ev.Call == nil {
				continue
			}
			switch ev.Kind {
			case "call":
				if call == nil {
					call = ev.Call
				}
			case "done":
				if doneEvent == nil {
					doneEvent = ev.Call
				}
			}
		}
		auditMu.Unlock()
		if call != nil && doneEvent != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if call == nil || doneEvent == nil {
		t.Fatalf("the audit never carried the call: %+v", events)
	}
	if call.Host != leaseEgressTestHost || call.Port != "443" || !call.Allowed || call.Refused != "" || call.Rule != egress.RuleHosts {
		t.Fatalf("the audited call %+v", call)
	}
	if doneEvent.Bytes <= 0 {
		t.Fatalf("the done event carried no bytes: %+v", doneEvent)
	}
}

// A test disconnects the control connection, drives more than 500
// audit-producing requests through the tunnel, then reconnects: the
// audit records replay oldest-dropped-first and the reconnect reports
// the exact count of records dropped while disconnected.
func TestLeaseEgressAuditDropsOldestFirstAndReportsTheCountOnReconnect(t *testing.T) {
	g := newLeaseEgressRig(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	ctx := context.Background()
	ctl := g.control(t)

	startRes, err := ctl.Start(ctx, LeaseEgressStartRequest{Hosts: []string{leaseEgressTestHost}})
	if err != nil || !startRes.Started {
		t.Fatalf("start: %+v, %v", startRes, err)
	}

	firstCtx, stopFirst := context.WithCancel(context.Background())
	firstResync := make(chan AuditResync, 1)
	go func() { _ = ctl.Audit(firstCtx, func(rs AuditResync) { firstResync <- rs }, nil) }()
	select {
	case rs := <-firstResync:
		if rs.Dropped != 0 {
			t.Fatalf("the first connection's resync already carried a drop: %+v", rs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no resync on the first connection")
	}
	stopFirst()

	const requests = 501
	for i := 0; i < requests; i++ {
		resp, err := g.client.Get(fmt.Sprintf("https://%s/x/%d", leaseEgressTestHost, i))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}

	secondCtx, stopSecond := context.WithCancel(context.Background())
	defer stopSecond()
	resyncCh := make(chan AuditResync, 1)
	var mu sync.Mutex
	var backlog []AuditEvent
	go func() {
		_ = ctl.Audit(secondCtx, func(rs AuditResync) { resyncCh <- rs }, func(ev AuditEvent) {
			mu.Lock()
			backlog = append(backlog, ev)
			mu.Unlock()
		})
	}()

	var rs AuditResync
	select {
	case rs = <-resyncCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no resync on reconnect")
	}

	total := int64(requests * 2) // a "call" and a "done" event per request
	wantDropped := total - int64(DefaultAuditBufferCap)
	if rs.Dropped != wantDropped {
		t.Fatalf("dropped %d, want %d", rs.Dropped, wantDropped)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(backlog)
		mu.Unlock()
		if n >= DefaultAuditBufferCap {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := append([]AuditEvent(nil), backlog...)
	mu.Unlock()
	if len(got) != DefaultAuditBufferCap {
		t.Fatalf("the backlog carried %d events, want %d", len(got), DefaultAuditBufferCap)
	}
	wantFirstSeq := uint64(total) - uint64(DefaultAuditBufferCap)
	for i, ev := range got {
		if ev.Seq != wantFirstSeq+uint64(i) {
			t.Fatalf("backlog[%d].Seq = %d, want %d; not oldest-surviving-first", i, ev.Seq, wantFirstSeq+uint64(i))
		}
	}
}
