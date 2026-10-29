package nodev1

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
