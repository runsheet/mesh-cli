package nodev1

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"projectmesh.io/mesh-cli/internal/harness/session/sessiontest"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// nodeOwnSecret is a model key in the node's own environment.
const nodeOwnSecret = "sk-ant-api03-the-nodes-own-key"

// clearModelEnv empties the node's model variables, whatever the
// machine running the tests has.
func clearModelEnv(t *testing.T) {
	t.Helper()
	for _, k := range modelAuthEnv {
		t.Setenv(k, "")
	}
	t.Setenv(ModelUpstreamEnv+"ANTHROPIC", "")
	t.Setenv(ModelUpstreamEnv+"OPENAI", "")
}

// fakeModel is a model API that records the credential each call
// carried and answers 200 to the one it accepts.
type fakeModel struct {
	srv    *httptest.Server
	accept func(r *http.Request) bool

	mu    sync.Mutex
	seen  []string // each call's Authorization|X-Api-Key
	dummy bool     // whether any call carried the session's dummy
}

func newFakeModel(t *testing.T, accept func(r *http.Request) bool) *fakeModel {
	m := &fakeModel{accept: accept}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		m.mu.Lock()
		m.seen = append(m.seen, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Api-Key"))
		for _, vs := range r.Header {
			for _, v := range vs {
				if strings.Contains(v, ModelDummy) {
					m.dummy = true
				}
			}
		}
		m.mu.Unlock()
		if r.URL.Path != "/v1/messages" || !m.accept(r) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"type":"error","error":{"type":"authentication_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeModel) calls() ([]string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seen...), m.dummy
}

// envValue is name's value in a spec's Env.
func envValue(env []string, name string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// callModel is the session calling its model as Claude Code does on a
// subscription: its dummy as a bearer, at its base URL.
func callModel(base, path, auth string) int {
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(`{"model":"claude-haiku"}`))
	req.Header.Set("Authorization", auth)
	req.Header.Set("X-Api-Key", ModelDummy)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// No model key in the session (ADR 001 amendment "model relay"): a
// session on the job's account, run by the real streaming session on
// the fake claude, has a whole environment that holds neither the
// account's secret nor the node's own key — only the dummy in
// CLAUDE_CODE_OAUTH_TOKEN, the relay as ANTHROPIC_BASE_URL and no
// non-essential traffic — and reaches its model through the relay,
// which the upstream sees carrying the account's real bearer and never
// the dummy. After the lease the relay's port is closed.
func TestTheSessionHoldsNoModelKey(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", nodeOwnSecret)
	t.Setenv("OPENAI_API_KEY", nodeOwnSecret+"-openai")
	out := filepath.Join(t.TempDir(), "claude")
	t.Setenv(sessiontest.Env, "1")
	t.Setenv("FAKE_CLAUDE_OUT", out)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	model := newFakeModel(t, func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+fakeModelSecret })
	g := newRig(t, &ClaudeSession{Binary: self, Logf: t.Logf})
	g.rt.ModelUpstreams = map[string]string{"anthropic": model.srv.URL}
	g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out + ".env")
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, kv := range env {
		if strings.Contains(kv, fakeModelSecret) || strings.Contains(kv, nodeOwnSecret) {
			t.Fatalf("the session's environment holds a model key: %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
	base := envValue(env, "ANTHROPIC_BASE_URL")
	if envValue(env, "CLAUDE_CODE_OAUTH_TOKEN") != ModelDummy || envValue(env, "ANTHROPIC_API_KEY") != "" || envValue(env, "OPENAI_API_KEY") != "" ||
		envValue(env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC") != "1" || !strings.HasPrefix(base, "http://127.0.0.1:") || !strings.HasSuffix(base, "/model/anthropic") {
		t.Fatalf("the session's model environment: base %q oauth %q api key set %v", base,
			envValue(env, "CLAUDE_CODE_OAUTH_TOKEN"), envValue(env, "ANTHROPIC_API_KEY") != "")
	}
	answer, _ := os.ReadFile(out + ".model")
	if !strings.HasPrefix(string(answer), "200 ") {
		t.Fatalf("the session's model call answered %q", answer)
	}
	seen, dummy := model.calls()
	if len(seen) != 1 || seen[0] != "Bearer "+fakeModelSecret+"|" || dummy {
		t.Fatalf("the model saw %d call(s), the dummy %v", len(seen), dummy)
	}
	if code := callModel(base, "/v1/messages", "Bearer "+ModelDummy); code != 0 {
		t.Fatalf("the relay still answers %d after the lease", code)
	}
}

// An upstream's 401 has the relay read the job's account again from
// Latchkey, once, and ask again with what it answers: the session sees
// only the answer to the fresh credential.
func TestTheRelayReadsTheAccountAgainOn401(t *testing.T) {
	clearModelEnv(t)
	const rotated = "sk-ant-oat01-rotated-model-secret"
	var g *rig
	model := newFakeModel(t, func(r *http.Request) bool {
		if r.Header.Get("Authorization") == "Bearer "+rotated {
			return true
		}
		// Latchkey refreshed the account's token since the lease read it
		g.latchkey.mu.Lock()
		g.latchkey.model.Secret = rotated
		g.latchkey.mu.Unlock()
		return false
	})
	var code int
	sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "s"}}
	sess.inspect = func(spec SessionSpec) {
		code = callModel(envValue(spec.Env, "ANTHROPIC_BASE_URL"), "/v1/messages", "Bearer "+envValue(spec.Env, "CLAUDE_CODE_OAUTH_TOKEN"))
	}
	g = newRig(t, sess)
	g.rt.ModelUpstreams = map[string]string{"anthropic": model.srv.URL}
	g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	seen, dummy := model.calls()
	if code != 200 || g.latchkey.modelReads != 2 || len(seen) != 2 || seen[1] != "Bearer "+rotated+"|" || dummy {
		t.Fatalf("answered %d after %d read(s); the model saw %d call(s), the dummy %v", code, g.latchkey.modelReads, len(seen), dummy)
	}
	if len(g.fake.Calls("finish")) != 1 {
		t.Fatal("the job did not finish")
	}
}

// A key in the node's own environment, on a brief with no
// credential_ref, is relayed the same way: the session holds the dummy
// in ANTHROPIC_API_KEY and the upstream sees the node's key as
// x-api-key. The relay carries the account to its own provider alone.
func TestTheNodesOwnKeyIsRelayed(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", nodeOwnSecret)
	model := newFakeModel(t, func(r *http.Request) bool {
		return r.Header.Get("X-Api-Key") == nodeOwnSecret && r.Header.Get("Authorization") == ""
	})
	var code, other int
	var spec SessionSpec
	sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "s"}}
	sess.inspect = func(s SessionSpec) {
		spec = s
		base := envValue(s.Env, "ANTHROPIC_BASE_URL")
		code = callModel(base, "/v1/messages", "Bearer "+ModelDummy)
		other = callModel(strings.TrimSuffix(base, "anthropic")+"openai", "/v1/chat/completions", "Bearer "+ModelDummy)
	}
	g := newRig(t, sess)
	g.rt.ModelUpstreams = map[string]string{"anthropic": model.srv.URL, "openai": model.srv.URL}
	g.offer(t, func(b *meshapi.Brief) { b.CredentialRef = "" })
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if envValue(spec.Env, "ANTHROPIC_API_KEY") != ModelDummy || !slices.Contains(spec.Unset, "ANTHROPIC_API_KEY") || strings.Contains(fmt.Sprint(spec), nodeOwnSecret) {
		t.Fatalf("the session's env %v unset %v", spec.Env, spec.Unset)
	}
	seen, dummy := model.calls()
	if code != 200 || len(seen) != 1 || seen[0] != "|"+nodeOwnSecret || dummy || g.latchkey.modelReads != 0 {
		t.Fatalf("answered %d; the model saw %d call(s), the dummy %v; %d for-job read(s)", code, len(seen), dummy, g.latchkey.modelReads)
	}
	if other != http.StatusForbidden {
		t.Fatalf("the relay carried an anthropic key to openai: %d", other)
	}
}

// A job account naming openai and a base_url of its own — an
// OpenAI-compatible gateway (ADR-050) — has the relay carry the
// account's real key, as a bearer, and its static header there, never
// to api.openai.com; the session's own environment holds neither. The
// fake gateway is plain http on loopback, which the relay allows only
// because it is on the origin of the operator's own upstream
// (accountUpstream); the account's path on it, not the operator's, is
// where the call goes.
func TestAnOpenAICompatibleAccountReachesItsOwnBaseURL(t *testing.T) {
	clearModelEnv(t)
	const secret = "sk-fake-openai-compatible-secret"
	var authHdr, orgHdr string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		authHdr, orgHdr = r.Header.Get("Authorization"), r.Header.Get("X-Org")
		if r.URL.Path != "/gw/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[]}`)
	}))
	t.Cleanup(upstream.Close)

	var code int
	var spec SessionSpec
	sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "s"}}
	sess.inspect = func(s SessionSpec) {
		spec = s
		base := envValue(s.Env, "OPENAI_BASE_URL")
		code = callModel(base, "/chat/completions", "Bearer "+ModelDummy)
	}
	g := newRig(t, sess)
	g.latchkey.model = modelCredential{
		Secret: secret, Vendor: "openai", Kind: "api_key", AccountRef: "acct-fake",
		BaseURL: upstream.URL + "/gw", Headers: map[string]string{"X-Org": "acme-org"},
	}
	g.rt.ModelUpstreams = map[string]string{"openai": upstream.URL + "/operator"}
	g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if code != 200 {
		t.Fatalf("the model call answered %d", code)
	}
	if authHdr != "Bearer "+secret || orgHdr != "acme-org" {
		t.Fatalf("the upstream saw Authorization %q X-Org %q", authHdr, orgHdr)
	}
	for _, kv := range spec.Env {
		if strings.Contains(kv, secret) {
			t.Fatalf("the session's env holds the account's secret: %s", kv)
		}
	}
	if envValue(spec.Env, "OPENAI_API_KEY") != ModelDummy {
		t.Fatalf("the session's OPENAI_API_KEY is %q, not the dummy", envValue(spec.Env, "OPENAI_API_KEY"))
	}
}

// A job account naming anthropic and a base_url of its own — an
// Anthropic Messages-compatible gateway, such as OpenRouter, which needs
// no relay-side translation (ADR-050) — has the relay carry the
// account's key as X-Api-Key there, never to api.anthropic.com.
func TestAnAnthropicCompatibleAccountReachesItsOwnBaseURL(t *testing.T) {
	clearModelEnv(t)
	const secret = "sk-fake-anthropic-compatible-secret"
	var apiKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		apiKey = r.Header.Get("X-Api-Key")
		if r.URL.Path != "/gw/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	t.Cleanup(upstream.Close)

	var code int
	sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "s"}}
	sess.inspect = func(s SessionSpec) {
		base := envValue(s.Env, "ANTHROPIC_BASE_URL")
		code = callModel(base, "/v1/messages", "Bearer "+ModelDummy)
	}
	g := newRig(t, sess)
	g.latchkey.model = modelCredential{
		Secret: secret, Vendor: "anthropic", Kind: "api_key", AccountRef: "acct-fake",
		BaseURL: upstream.URL + "/gw",
	}
	g.rt.ModelUpstreams = map[string]string{"anthropic": upstream.URL}
	g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if code != 200 || apiKey != secret {
		t.Fatalf("answered %d, X-Api-Key %q", code, apiKey)
	}
}

// The relay's environment for each kind it carries: the harness's base
// URL on the relay and the dummy in the variable it reads.
func TestRelayEnv(t *testing.T) {
	for _, c := range []struct {
		vendor, kind string
		want         []string
	}{
		{"anthropic", "subscription", []string{"ANTHROPIC_BASE_URL=http://r/model/anthropic", "CLAUDE_CODE_OAUTH_TOKEN=" + ModelDummy, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}},
		{"anthropic", "api_key", []string{"ANTHROPIC_BASE_URL=http://r/model/anthropic", "ANTHROPIC_API_KEY=" + ModelDummy, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}},
		{"openai", "api_key", []string{"OPENAI_BASE_URL=http://r/model/openai/v1", "OPENAI_API_KEY=" + ModelDummy, "CODEX_API_KEY=" + ModelDummy}},
	} {
		a, ok := authOf(c.vendor, c.kind)
		if got := a.relayEnv("http://r"); !ok || !slices.Equal(got, c.want) {
			t.Errorf("%s/%s: %v", c.vendor, c.kind, got)
		}
	}
}

// A ChatGPT Pro subscription travels the relay's OpenAI route the same
// way an OpenAI API key does: OPENAI_BASE_URL at the relay and the
// dummy in OPENAI_API_KEY and CODEX_API_KEY. modelCredential.auth() no
// longer refuses the vendor outright, so a job on a ChatGPT Pro account
// reaches the relay's upstream rather than being refused before any
// session runs.
func TestModelRelayChatGPTPro(t *testing.T) {
	a, ok := authOf("openai", "oauth_token")
	if !ok {
		t.Fatal("authOf(openai, oauth_token) refused")
	}
	want := []string{"OPENAI_BASE_URL=http://r/model/openai/v1", "OPENAI_API_KEY=" + ModelDummy, "CODEX_API_KEY=" + ModelDummy}
	if got := a.relayEnv("http://r"); !slices.Equal(got, want) {
		t.Fatalf("relayEnv = %v, want %v", got, want)
	}
	m := &modelCredential{Secret: "s", Vendor: "openai", Kind: "oauth_token", AccountRef: "acct-1"}
	if _, err := m.auth(); err != nil {
		t.Fatalf("modelCredential.auth() refused a ChatGPT Pro account: %v", err)
	}
}

// An account's own base_url is Latchkey's answer, so the relay holds it
// before sending the account's credential there: it must be https and
// its host must resolve only to public addresses — never loopback,
// link-local (the cloud metadata address), private or CGNAT — unless it
// is on the origin of one of the operator's own upstreams
// (MESH_MODEL_UPSTREAM_*). A refused base_url refuses the lease's model
// route by name; lookups are the test's own, never the network.
func TestAnAccountBaseURLIsHeldToAPublicHTTPSHost(t *testing.T) {
	answers := map[string][]string{
		"gateway.example":  {"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"},
		"internal.example": {"10.0.0.7"},
		"mixed.example":    {"93.184.216.34", "127.0.0.1"},
		"metadata.example": {"169.254.169.254"},
	}
	resolve := func(_ context.Context, host string) ([]net.IPAddr, error) {
		a, ok := answers[host]
		if !ok {
			return nil, fmt.Errorf("no such host %s", host)
		}
		var out []net.IPAddr
		for _, ip := range a {
			out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
		}
		return out, nil
	}
	for _, c := range []struct {
		base     string
		operator map[string]string
		refused  string
	}{
		{"https://gateway.example/v1", nil, ""},
		{"https://93.184.216.34/v1", nil, ""},
		{"http://gateway.example/v1", nil, "is not https"},
		{"https://127.0.0.1:8443", nil, "not a public address"},
		{"https://[::1]/v1", nil, "not a public address"},
		{"https://169.254.169.254/latest", nil, "not a public address"},
		{"https://metadata.example/", nil, "not a public address"},
		{"https://10.1.2.3/v1", nil, "not a public address"},
		{"https://internal.example/v1", nil, "not a public address"},
		{"https://mixed.example/v1", nil, "not a public address"},
		{"https://nowhere.example/v1", nil, "does not resolve"},
		{"https://user:pw@gateway.example/v1", nil, "not a URL with a host"},
		{"gateway.example/v1", nil, "not a URL with a host"},
		// the operator's own upstream's origin is the operator's choice
		{"http://127.0.0.1:9000/gw", map[string]string{"openai": "http://127.0.0.1:9000"}, ""},
		{"https://10.1.2.3/v1", map[string]string{"anthropic": "https://10.1.2.3"}, ""},
		// another port or scheme on that host is not
		{"http://127.0.0.1:9001/gw", map[string]string{"openai": "http://127.0.0.1:9000"}, "is not https"},
		{"https://127.0.0.1:9000/gw", map[string]string{"openai": "http://127.0.0.1:9000"}, "not a public address"},
	} {
		a := &sessionAccount{
			auth:     modelAuth{provider: "openai", header: "Authorization", bearer: true, envVar: "OPENAI_API_KEY", baseURL: c.base},
			cred:     &modelCredential{Secret: "s", Vendor: "openai", Kind: "api_key", AccountRef: "acct-1", BaseURL: c.base},
			upstream: c.base,
			resolve:  resolve,
		}
		ups, err := a.upstreamsWith(c.operator)
		if c.refused == "" {
			if err != nil || ups["openai"] != c.base {
				t.Errorf("%s (operator %v): refused or not routed: %v, %v", c.base, c.operator, err, ups)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.refused) {
			t.Errorf("%s (operator %v): want refused %q, got %v", c.base, c.operator, c.refused, err)
		}
	}
}

// A lease whose account names a base_url the relay does not trust —
// here the cloud metadata address — never starts a session: the lease
// is handed back and the account's credential reaches nothing.
func TestALeaseWhoseAccountBaseURLIsPrivateRunsNoSession(t *testing.T) {
	clearModelEnv(t)
	ran := false
	sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "s"}}
	sess.inspect = func(SessionSpec) { ran = true }
	g := newRig(t, sess)
	g.latchkey.model = modelCredential{
		Secret: "sk-fake", Vendor: "anthropic", Kind: "api_key", AccountRef: "acct-fake",
		BaseURL: "https://169.254.169.254/v1",
	}
	g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("a session ran on an account whose base_url is the metadata address")
	}
	calls := g.fake.Calls("hand-back")
	if len(calls) == 0 {
		t.Fatal("the lease was not handed back")
	}
	var hb meshapi.HandBackRequest
	json.Unmarshal(calls[len(calls)-1].Body, &hb)
	if !strings.Contains(hb.Reason, "169.254.169.254") || !strings.Contains(hb.Reason, "not a public address") || strings.Contains(hb.Reason, "sk-fake") {
		t.Fatalf("the hand-back's reason %q", hb.Reason)
	}
}

// A reread of the job's account that names another base_url than the
// one the lease's route was checked for is refused, not sent the key.
func TestARereadNamingAnotherBaseURLIsRefused(t *testing.T) {
	first := &modelCredential{Secret: "s1", Vendor: "openai", Kind: "api_key", AccountRef: "acct-1", BaseURL: "https://gateway.example/v1"}
	a1, err := first.auth()
	if err != nil {
		t.Fatal(err)
	}
	a := &sessionAccount{auth: a1, cred: first, upstream: a1.baseURL,
		reread: func(context.Context) (*modelCredential, error) {
			c := *first
			c.Secret, c.BaseURL = "s2", "https://10.0.0.7/v1"
			return &c, nil
		}}
	if _, err := a.credential(context.Background(), "openai", true); err == nil || !strings.Contains(err.Error(), "another base_url") {
		t.Fatalf("a reread naming another base_url: %v", err)
	}
}

// A base_url that passes its check and then rebinds — public when the
// lease opens, the node's loopback at the dial — is never dialled on the
// private address: the relay's transport resolves the host again at the
// dial, checks every answer, and dials only those it checked.
func TestARebindingBaseURLNeverReachesAPrivateAddress(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ConnState = func(net.Conn, http.ConnState) { conns.Add(1) }
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	var lookups atomic.Int32
	var dialled []string
	var mu sync.Mutex
	base := "https://rebind.example:" + port + "/v1"
	a := &sessionAccount{
		auth:     modelAuth{provider: "openai", header: "Authorization", bearer: true, baseURL: base},
		cred:     &modelCredential{Secret: "s", Vendor: "openai", Kind: "api_key", AccountRef: "acct-1", BaseURL: base},
		upstream: base,
		resolve: func(_ context.Context, host string) ([]net.IPAddr, error) {
			if lookups.Add(1) == 1 {
				return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
			}
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		},
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			dialled = append(dialled, addr)
			mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	if _, err := a.upstreamsWith(nil); err != nil {
		t.Fatalf("the public answer at the check was refused: %v", err)
	}
	m := a.model(map[string]string{"openai": base}, t.Logf, "lse_1")
	tr, ok := m.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the account's route has no pinned transport: %T", m.Transport)
	}
	tr.TLSClientConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	resp, err := (&http.Client{Transport: tr}).Get(base + "/chat/completions")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("the rebound base_url answered %d", resp.StatusCode)
	}
	if !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("the dial's refusal: %v", err)
	}
	if lookups.Load() < 2 {
		t.Fatalf("the dial did not resolve the host again (%d lookups)", lookups.Load())
	}
	if len(dialled) != 0 || conns.Load() != 0 {
		t.Fatalf("the relay dialled %v; the private server saw %d connections", dialled, conns.Load())
	}
}

// A pinned base_url is dialled on the address checked at the dial, and
// TLS still names and verifies its hostname, not the address.
func TestAPinnedBaseURLKeepsItsHostnameForTLS(t *testing.T) {
	var sni string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sni = r.TLS.ServerName
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	// httptest's certificate names example.com
	base := "https://example.com:" + port + "/v1"
	var dialled []string
	a := &sessionAccount{
		auth:     modelAuth{provider: "openai", header: "Authorization", bearer: true, baseURL: base},
		cred:     &modelCredential{Secret: "s", Vendor: "openai", Kind: "api_key", AccountRef: "acct-1", BaseURL: base},
		upstream: base,
		resolve: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
		},
		// the checked public address stands in for the fake's loopback
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialled = append(dialled, addr)
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}
	if _, err := a.upstreamsWith(nil); err != nil {
		t.Fatal(err)
	}
	tr := a.model(nil, t.Logf, "lse_1").Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	resp, err := (&http.Client{Transport: tr}).Get(base + "/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(dialled) != 1 || dialled[0] != "93.184.216.34:"+port || sni != "example.com" {
		t.Fatalf("dialled %v, SNI %q", dialled, sni)
	}
}
