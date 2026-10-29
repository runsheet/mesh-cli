package nodev1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/meshapi/meshapitest"
)

// product is a product's MCP server: it records each call and answers
// a JSON-RPC result.
type product struct {
	mu    sync.Mutex
	calls []string
	auth  []string
}

func (p *product) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var msg struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	json.Unmarshal(b, &msg)
	p.mu.Lock()
	p.calls = append(p.calls, msg.Method+" "+msg.Params.Name)
	p.auth = append(p.auth, r.Header.Get("Authorization"))
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%v,"result":{"content":[{"type":"text","text":"ok"}]}}`, msg.ID)
}

// stubSession stands in for claude: it calls each tool of calls on the
// server its spec names, through the lease's sessionproxy socket as the
// session's mcp-relay would, then waits for hold (if set) and answers.
type stubSession struct {
	calls  []string // server/tool
	hold   chan struct{}
	err    error
	result SessionResult

	mu    sync.Mutex
	specs []SessionSpec
}

func (s *stubSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	s.mu.Unlock()
	for _, c := range s.calls {
		server, tool, _ := strings.Cut(c, "/")
		if err := callThroughProxy(ctx, spec.Servers[server], tool); err != nil {
			return SessionResult{}, err
		}
	}
	if s.hold != nil {
		select {
		case <-s.hold:
		case <-ctx.Done():
			return SessionResult{SessionID: "sess-held"}, ctx.Err()
		}
	}
	return s.result, s.err
}

// callThroughProxy posts one tools/call to the entry's socket and server.
func callThroughProxy(ctx context.Context, entry any, tool string) error {
	sock, name, ok := harness.ProxiedOf(entry)
	if !ok {
		return fmt.Errorf("not a proxied entry: %v", entry)
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, tool)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://proxy/mcp/"+name, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer from-the-session")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("proxy answered %d", resp.StatusCode)
	}
	return nil
}

type rig struct {
	fake       *meshapitest.Fake
	product    *product
	productURL string
	rt         *Runtime
	work       string
}

func newRig(t *testing.T, sess Session) *rig {
	t.Helper()
	fake := meshapitest.NewFake(t)
	mesh := httptest.NewServer(fake)
	t.Cleanup(mesh.Close)
	prod := &product{}
	ps := httptest.NewServer(prod)
	t.Cleanup(ps.Close)
	work := t.TempDir()
	rt := &Runtime{
		API:        &meshapi.Client{BaseURL: mesh.URL, Token: func(context.Context) (string, error) { return "machine", nil }},
		Node:       meshapi.RegisterRequest{Name: "test-node", Version: "v0.0.0-test", CapacityTotal: meshapi.Capacity{CPUs: 4, MemGB: 8, Slots: 1}, SigningKey: "ed25519:AAAA"},
		WorkRoot:   work,
		MeshBinary: "/usr/local/bin/mesh",
		Session:    sess,
		Once:       true,
		Logf:       t.Logf,
		Backoff:    10 * time.Millisecond,
	}
	return &rig{fake: fake, product: prod, productURL: ps.URL + "/mcp", rt: rt, work: work}
}

func (g *rig) offer(t *testing.T, mut func(*meshapi.Brief)) string {
	return g.fake.Offer(func(b *meshapi.Brief) {
		b.Servers = []meshapi.Server{{Name: "conform", URL: g.productURL}}
		if mut != nil {
			mut(b)
		}
	})
}

func run(t *testing.T, g *rig, ctx context.Context) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the runtime did not stop")
		return nil
	}
}

// The slice: register, take a lease, run one session that reaches the
// product's MCP server through the lease's sessionproxy — the proxy, not
// the session, says who calls upstream — then post usage and finish with
// the session's ref; the lease's directory and socket are gone after.
func TestRunTakesALeaseRunsOneSessionAndFinishes(t *testing.T) {
	sess := &stubSession{
		calls:  []string{"conform/brief", "conform/submit"},
		result: SessionResult{SessionID: "sess-1", Text: "submitted", CostUSD: 0.02, Turns: 3, Usage: &harness.Usage{Model: "claude-haiku-4-5", InputTokens: 100, OutputTokens: 10}},
	}
	g := newRig(t, sess)
	job := g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q", lease, g.fake.Ended(lease))
	}
	if len(g.fake.Calls("register")) != 1 {
		t.Fatal("no registration")
	}
	fin := g.fake.Calls("finish")
	var req meshapi.FinishRequest
	json.Unmarshal(fin[0].Body, &req)
	if req.SessionRef != "sess-1" || !strings.Contains(string(req.Result), "submitted") || fin[0].Auth != "Bearer machine" {
		t.Fatalf("finish %s", fin[0].Body)
	}
	var usage meshapi.UsageRequest
	json.Unmarshal(g.fake.Calls("usage")[0].Body, &usage)
	if len(usage.Usage) != 1 || usage.Usage[0].Model != "claude-haiku-4-5" || usage.Turns != 3 {
		t.Fatalf("usage %+v", usage)
	}
	g.product.mu.Lock()
	calls, auth := g.product.calls, g.product.auth
	g.product.mu.Unlock()
	if strings.Join(calls, ",") != "tools/call brief,tools/call submit" {
		t.Fatalf("product calls %v", calls)
	}
	for _, a := range auth {
		if a != "" {
			t.Fatalf("the session's own Authorization reached the product: %q", a)
		}
	}
	spec := sess.specs[0]
	if spec.Model != "haiku" || !strings.Contains(spec.Prompt, "`brief`") || !strings.Contains(spec.Prompt, "`submit`") {
		t.Fatalf("spec model %q prompt %q", spec.Model, spec.Prompt)
	}
	sock, _, _ := harness.ProxiedOf(spec.Servers["conform"])
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the proxy's socket is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(g.work, "leases", lease)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lease's directory is still there: %v", err)
	}
}

// A session that cannot run hands the job back with cannot_run and its
// reason; it spends no attempt.
func TestASessionThatCannotRunHandsBack(t *testing.T) {
	g := newRig(t, &stubSession{err: errors.New("claude: not logged in"), result: SessionResult{SessionID: "sess-2"}})
	job := g.offer(t, nil)
	run(t, g, context.Background())
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "hand-back" {
		t.Fatalf("ended %q", g.fake.Ended(lease))
	}
	var hb meshapi.HandBackRequest
	json.Unmarshal(g.fake.Calls("hand-back")[0].Body, &hb)
	if hb.Cause != meshapi.CauseCannotRun || !strings.Contains(hb.Reason, "not logged in") || hb.SessionRef != "sess-2" {
		t.Fatalf("hand-back %+v", hb)
	}
	if len(g.fake.Calls("finish")) != 0 {
		t.Fatal("finished too")
	}
}

// The lease is renewed while the session runs; once Mesh says it is no
// longer the node's, the session is stopped and nothing is reported.
func TestALeaseMeshEndedStopsTheSessionWithoutAReport(t *testing.T) {
	sess := &stubSession{hold: make(chan struct{})}
	g := newRig(t, sess)
	job := g.offer(t, func(b *meshapi.Brief) { b.RenewEveryS = 1 })
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(g.fake.Calls("renew")) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if len(g.fake.Calls("renew")) == 0 {
		t.Fatal("the lease was never renewed")
	}
	g.fake.ExpireLease(g.fake.LeaseOf(job))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the session outlived its lease")
	}
	for _, r := range []string{"finish", "fail", "hand-back", "usage"} {
		if n := len(g.fake.Calls(r)); n != 0 {
			t.Fatalf("%d %s calls after the lease ended", n, r)
		}
	}
}

// A heartbeat that names the lease in cancel stops its session; nothing
// is reported.
func TestHeartbeatCancelStopsTheSession(t *testing.T) {
	sess := &stubSession{hold: make(chan struct{})}
	g := newRig(t, sess)
	g.rt.HeartbeatEvery = 50 * time.Millisecond
	job := g.offer(t, nil)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	deadline := time.Now().Add(10 * time.Second)
	for g.fake.LeaseOf(job) == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	g.fake.CancelLease(g.fake.LeaseOf(job))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled session went on")
	}
	if n := len(g.fake.Calls("hand-back")) + len(g.fake.Calls("finish")); n != 0 {
		t.Fatalf("%d reports after a cancel", n)
	}
	var hb meshapi.HeartbeatRequest
	json.Unmarshal(g.fake.Calls("heartbeat")[0].Body, &hb)
	if len(hb.Leases) != 1 || hb.Leases[0].LeaseID != g.fake.LeaseOf(job) {
		t.Fatalf("heartbeat leases %+v", hb.Leases)
	}
}

// A node stopping mid-session hands the lease back with node_restart.
func TestTheNodeStoppingHandsBack(t *testing.T) {
	sess := &stubSession{hold: make(chan struct{})}
	g := newRig(t, sess)
	job := g.offer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(sess.specsSoFar()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	lease := g.fake.LeaseOf(job)
	var hb meshapi.HandBackRequest
	calls := g.fake.Calls("hand-back")
	if len(calls) != 1 {
		t.Fatalf("hand-backs %d", len(calls))
	}
	json.Unmarshal(calls[0].Body, &hb)
	if g.fake.Ended(lease) != "hand-back" || hb.Cause != meshapi.CauseNodeRestart {
		t.Fatalf("ended %q, %+v", g.fake.Ended(lease), hb)
	}
}

func (s *stubSession) specsSoFar() []SessionSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SessionSpec(nil), s.specs...)
}

// A drain from the heartbeat stops the node taking more leases.
func TestDrainStopsTaking(t *testing.T) {
	g := newRig(t, &stubSession{})
	g.rt.Once = false
	g.rt.HeartbeatEvery = 20 * time.Millisecond
	g.fake.Drain()
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
}
