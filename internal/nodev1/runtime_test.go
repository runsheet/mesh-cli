package nodev1

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/meshapi/meshapitest"
)

// stubSession stands in for claude: it calls each tool of calls on the
// server its spec names, through the lease's sessionproxy socket as the
// session's mcp-relay would, then waits for hold (if set) and answers.
type stubSession struct {
	calls   []string // server/tool
	inspect func(SessionSpec)
	hold    chan struct{}
	err     error
	result  SessionResult

	mu    sync.Mutex
	specs []SessionSpec
}

func (s *stubSession) Run(ctx context.Context, spec SessionSpec) (SessionResult, error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	s.mu.Unlock()
	if s.inspect != nil {
		s.inspect(spec)
	}
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
	latchkey   *fakeLatchkey
	github     *fakeGitHub
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
	lk := newFakeLatchkey(t)
	fake.Session = lk.open
	gh := newFakeGitHub(t)
	prod := &product{l: lk}
	ps := httptest.NewServer(prod)
	t.Cleanup(ps.Close)
	prod.url = ps.URL + "/mcp"
	work := t.TempDir()
	_, nodeKey, _ := ed25519.GenerateKey(rand.Reader)
	rt := &Runtime{
		SigningKey:  nodeKey,
		API:         &meshapi.Client{BaseURL: mesh.URL, Token: func(context.Context) (string, error) { return "machine", nil }},
		Node:        meshapi.RegisterRequest{Name: "test-node", Version: "v0.0.0-test", CapacityTotal: meshapi.Capacity{CPUs: 4, MemGB: 8, Slots: 1}, SigningKey: "ed25519:AAAA"},
		WorkRoot:    work,
		MeshBinary:  "/usr/local/bin/mesh",
		Session:     sess,
		Once:        true,
		Logf:        t.Logf,
		Backoff:     10 * time.Millisecond,
		GitUpstream: gh.srv.URL,
	}
	return &rig{fake: fake, latchkey: lk, github: gh, product: prod, productURL: prod.url, rt: rt, work: work}
}

func (g *rig) offer(t *testing.T, mut func(*meshapi.Brief)) string {
	return g.fake.Offer(func(b *meshapi.Brief) {
		b.Servers = []meshapi.Server{{Name: "conform", URL: g.productURL}}
		b.Code.BaseSHA = g.github.head
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

// The slice (Mesh v1 PR 0.8b): register, take a lease, open its
// Latchkey session through Mesh with a fresh key's jkt, trade lksr_ for
// a DPoP mesh-session token, have the relay mint the git token, check
// the repository out through the relay with no credential anywhere in the
// checkout, run one session that reaches the product's MCP through the
// proxy on the DPoP token signed for the product's URL and git through
// the proxy's /git/ route, then finish with what the product's submit
// answered. After release the capability answers 404, the git token is
// revoked and Mesh holds the AccessRecords.
func TestRunTakesALeaseRunsOneSessionAndFinishes(t *testing.T) {
	var seen struct {
		readme, gitConfig, origin string
		env                       []string
		gitViaProxy               int
	}
	sess := &stubSession{
		calls:  []string{"conform/brief", "conform/submit"},
		result: SessionResult{SessionID: "sess-1", Text: "submitted", CostUSD: 0.02, Turns: 3, Usage: &harness.Usage{Model: "claude-haiku-4-5", InputTokens: 100, OutputTokens: 10}},
	}
	sess.inspect = func(spec SessionSpec) {
		b, _ := os.ReadFile(filepath.Join(spec.Workspace, "README.md"))
		seen.readme = string(b)
		c, _ := os.ReadFile(filepath.Join(spec.Workspace, ".git", "config"))
		seen.gitConfig = string(c)
		out, _ := exec.Command("git", "-C", spec.Workspace, "config", "remote.origin.url").Output()
		seen.origin = strings.TrimSpace(string(out))
		seen.env = harness.SessionEnviron(append(os.Environ(), "GH_TOKEN=node-own"), harness.Job{Env: spec.Env, Unset: spec.Unset})
		sock, _, _ := harness.ProxiedOf(spec.Servers["conform"])
		seen.gitViaProxy = getViaSocket(sock, "/git/acme/fixture.git/info/refs?service=git-upload-pack")
	}
	g := newRig(t, sess)
	job := g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	lease := g.fake.LeaseOf(job)
	if g.fake.Ended(lease) != "finish" {
		t.Fatalf("lease %s ended %q; hand-backs %v", lease, g.fake.Ended(lease), g.fake.Calls("hand-back"))
	}

	// the session and its key
	var sreq meshapi.SessionRequest
	json.Unmarshal(g.fake.Calls("session")[0].Body, &sreq)
	if sreq.JKT == "" {
		t.Fatal("the session was opened with no jkt")
	}
	minted, revoked, lkFailures := g.latchkey.snapshot()
	if len(lkFailures) > 0 {
		t.Fatalf("Latchkey refused: %v", lkFailures)
	}

	// the checkout, through the relay, with no credential in it
	if seen.readme != "the fixture\n" {
		t.Fatalf("the checkout read %q", seen.readme)
	}
	if !strings.HasPrefix(seen.origin, "http://127.0.0.1:") || !strings.Contains(seen.origin, "/j/") {
		t.Fatalf("origin is %q, not the relay", seen.origin)
	}
	for _, secret := range append(minted, "lksr_", "mst_", "ghs_", "helper", "askpass") {
		if strings.Contains(strings.ToLower(seen.gitConfig), strings.ToLower(secret)) {
			t.Fatalf("the checkout's git config names %q:\n%s", secret, seen.gitConfig)
		}
	}
	for _, kv := range seen.env {
		for _, secret := range []string{"lksr_", "mst_", "ghs_", "GH_TOKEN=", "GIT_ASKPASS="} {
			if strings.Contains(kv, secret) {
				t.Fatalf("the session's environment carries %q", kv)
			}
		}
	}
	if seen.gitViaProxy != 200 {
		t.Fatalf("git through the proxy's /git/ route: %d", seen.gitViaProxy)
	}
	if len(minted) == 0 {
		t.Fatal("no git token was minted")
	}
	for _, a := range g.github.auths() {
		if a != basicAuth(minted[0]) && a != basicAuth(minted[len(minted)-1]) {
			t.Fatalf("GitHub saw %q, not the minted token", a)
		}
	}

	// the product, on the DPoP session token
	calls, pFailures := g.product.snapshot()
	if len(pFailures) > 0 {
		t.Fatalf("the product refused: %v", pFailures)
	}
	if strings.Join(calls, ",") != "tools/call brief,tools/call submit" {
		t.Fatalf("product calls %v", calls)
	}

	// the finish carries the product's answer to submit
	fin := g.fake.Calls("finish")
	var req meshapi.FinishRequest
	json.Unmarshal(fin[0].Body, &req)
	if req.SessionRef != "sess-1" || string(req.Result) != `{"sha256":"5f0c"}` || fin[0].Auth != "Bearer machine" {
		t.Fatalf("finish %s", fin[0].Body)
	}
	var usage meshapi.UsageRequest
	json.Unmarshal(g.fake.Calls("usage")[0].Body, &usage)
	if len(usage.Models) != 1 || usage.Models[0].Model != "claude-haiku-4-5" || usage.Turns != 3 || usage.Harness != "claude-code" || usage.AccountRef != "acct-fake" {
		t.Fatalf("usage %+v", usage)
	}

	// release: 404, revoked, audited
	if c := getURL(seen.origin + "/info/refs?service=git-upload-pack"); c != http.StatusNotFound {
		t.Fatalf("the capability answered %d after release", c)
	}
	if strings.Join(revoked, ",") != strings.Join(minted, ",") {
		t.Fatalf("minted %v, revoked %v", minted, revoked)
	}
	var access meshapi.AccessRequest
	json.Unmarshal(g.fake.Calls("access")[0].Body, &access)
	if len(access.Records) < 2 || access.Records[0].Kind != "git" || access.Records[0].Repo != "acme/fixture" || access.Records[0].Outcome != "ok" {
		t.Fatalf("access records %+v", access.Records)
	}
	if len(g.rt.relay.Leases()) != 0 {
		t.Fatalf("grants left open: %v", g.rt.relay.Leases())
	}
	spec := sess.specs[0]
	// the brief's credential_ref: the session runs on the job's account,
	// its secret in the session's environment alone and masked
	if !slices.Contains(spec.Env, "CLAUDE_CODE_OAUTH_TOKEN="+fakeModelSecret) || !slices.Contains(spec.Secrets, fakeModelSecret) ||
		!slices.Contains(spec.Unset, "ANTHROPIC_API_KEY") || !slices.Contains(spec.Unset, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("the session's account env %v unset %v", spec.Env, spec.Unset)
	}
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

func basicAuth(tok string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))
}

func getURL(url string) int {
	resp, err := http.Get(url)
	if err != nil {
		return 0
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func getViaSocket(sock, path string) int {
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := c.Get("http://proxy" + path)
	if err != nil {
		return 0
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// Mesh refusing the lease's session hands the job back: the node cannot
// run it without its key to Latchkey.
func TestASessionMeshRefusesHandsBack(t *testing.T) {
	g := newRig(t, &stubSession{})
	g.fake.Session = func(string, string) (*meshapi.SessionCredential, int) { return nil, http.StatusServiceUnavailable }
	job := g.offer(t, nil)
	run(t, g, context.Background())
	var hb meshapi.HandBackRequest
	json.Unmarshal(g.fake.Calls("hand-back")[0].Body, &hb)
	if g.fake.Ended(g.fake.LeaseOf(job)) != "hand-back" || hb.Cause != meshapi.CauseCannotRun || !strings.Contains(hb.Reason, "session") {
		t.Fatalf("%+v", hb)
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

// A heartbeat that names the lease in cancel stops its session, and the
// lease is handed back with cause cancelled.
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
	assertCancelledHandBack(t, g, job, "Mesh cancelled the lease (heartbeat)")
	var hb meshapi.HeartbeatRequest
	json.Unmarshal(g.fake.Calls("heartbeat")[0].Body, &hb)
	if len(hb.Leases) != 1 || hb.Leases[0].LeaseID != g.fake.LeaseOf(job) {
		t.Fatalf("heartbeat leases %+v", hb.Leases)
	}
}

// A cancel on the node's event stream stops the session at once — long
// before any heartbeat — and the lease is handed back with cause
// cancelled and the cancel's reason (projectmesh-io/api wave 1.5).
func TestStreamCancelStopsTheSession(t *testing.T) {
	sess := &stubSession{hold: make(chan struct{})}
	g := newRig(t, sess)
	g.rt.HeartbeatEvery = time.Hour
	job := g.offer(t, nil)
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(context.Background()) }()
	deadline := time.Now().Add(10 * time.Second)
	for (len(sess.specsSoFar()) == 0 || g.fake.Streams() == 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := g.fake.StreamCancel(g.fake.LeaseOf(job), "superseded", "a newer job replaced it"); n != 1 {
		t.Fatalf("the cancel reached %d streams", n)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled session went on")
	}
	assertCancelledHandBack(t, g, job, "Mesh cancelled the job (superseded): a newer job replaced it")
	if n := len(g.fake.Calls("heartbeat")); n != 0 {
		t.Fatalf("%d heartbeats: the stream, not a heartbeat, stopped the session", n)
	}
}

// A drain on the event stream stops the node taking more leases.
func TestStreamDrainStopsTaking(t *testing.T) {
	g := newRig(t, &stubSession{})
	g.rt.Once = false
	g.rt.HeartbeatEvery = time.Hour
	go func() {
		for g.fake.Stream(meshapi.NodeEvent{Event: meshapi.EventDrain, Data: json.RawMessage(`{}`)}) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
}

// assertCancelledHandBack: the job's lease was handed back once, with
// cause cancelled and reason, and nothing else was reported.
func assertCancelledHandBack(t *testing.T, g *rig, job, reason string) {
	t.Helper()
	calls := g.fake.Calls("hand-back")
	if len(calls) != 1 || len(g.fake.Calls("finish"))+len(g.fake.Calls("fail")) != 0 {
		t.Fatalf("%d hand-backs, %d finishes, %d failures after a cancel", len(calls), len(g.fake.Calls("finish")), len(g.fake.Calls("fail")))
	}
	var hb meshapi.HandBackRequest
	json.Unmarshal(calls[0].Body, &hb)
	if calls[0].LeaseID != g.fake.LeaseOf(job) || hb.Cause != meshapi.CauseCancelled || hb.Reason != reason {
		t.Fatalf("hand-back %s %+v", calls[0].LeaseID, hb)
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

// Once registered, the node offers itself to each connection it was
// given before it takes anything (runsheet ADR 066).
func TestRunOffersItsConnections(t *testing.T) {
	g := newRig(t, &stubSession{})
	g.rt.Once = false
	g.rt.HeartbeatEvery = 20 * time.Millisecond
	g.rt.Connect = []meshapi.ConnectRequest{{Tenant: "acme", Product: "conform"}, {Tenant: "globex", Product: "conform"}}
	g.fake.Drain()
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := g.fake.Calls("connect")
	if len(calls) != 2 {
		t.Fatalf("connect calls %d", len(calls))
	}
	var first meshapi.ConnectRequest
	if json.Unmarshal(calls[0].Body, &first) != nil || first.Tenant != "acme" || first.Product != "conform" {
		t.Fatalf("first connect %s", calls[0].Body)
	}
}
