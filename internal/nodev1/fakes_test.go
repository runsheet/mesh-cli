package nodev1

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// fakeLatchkey speaks what projectmesh-io/api's internal/latchkeytest
// does for a lease: the session-token request on lksr_ with a DPoP proof,
// and the for-job source mint and revoke on the DPoP-bound session token.
type fakeLatchkey struct {
	srv *httptest.Server
	v   dpoptest.Verifier

	mu       sync.Mutex
	jkt      map[string]string // session id → the jkt Mesh opened it for
	creds    map[string]string // lksr_ → session id
	tokens   map[string]string // session token → jkt
	leaseOf  map[string]string // session id or session token → the lease it is for
	minted   []string
	access   []string // each mint's access, read or write
	revoked  []string
	failures []string
	api      []string // each proxied call: METHOD vendor/path
	slots    []string // each slot call: binding method [tool]
	mints    []SourceRequest
	// model is the for-job model credential the fake answers; refuseModel,
	// when set, is why it refuses instead; modelReads counts the reads
	model       modelCredential
	refuseModel string
	modelReads  int
	// refuseSource, when set, is why the source mint refuses
	refuseSource string
	// signingMints counts the sigstore-audience ID tokens minted
	signingMints int
	n            int
	// carried are the for-job credentials the fake answers by ref (a
	// Secret's or a connection's child, inject.go), refuseCarried the
	// refusals it answers in their place, {status, code}, and asked
	// each for-job ask's ref in order
	carried       map[string]forJobCredential
	refuseCarried map[string]carriedRefusal
	asked         []string
	// releases counts the for-job releases; refuseRelease, set, is the
	// status it answers instead; onRelease hears each as it comes
	releases      int
	refuseRelease int
	onRelease     func()
	// fed is the federation-token route's state (federation_test.go)
	fed fakeFederation
}

// carriedRefusal is a for-job refusal: Latchkey's status and code.
type carriedRefusal struct {
	status int
	code   string
}

// setCarried has the fake answer ref with c from now on.
func (l *fakeLatchkey) setCarried(ref string, c forJobCredential) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.carried == nil {
		l.carried = map[string]forJobCredential{}
	}
	l.carried[ref] = c
}

// asks counts the for-job asks of ref.
func (l *fakeLatchkey) asks(ref string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, a := range l.asked {
		if a == ref {
			n++
		}
	}
	return n
}

// fakeModelSecret is the fake account's secret.
const fakeModelSecret = "sk-ant-oat01-fake-model-secret"

func newFakeLatchkey(t *testing.T) *fakeLatchkey {
	l := &fakeLatchkey{jkt: map[string]string{}, creds: map[string]string{}, tokens: map[string]string{}, leaseOf: map[string]string{},
		model: modelCredential{Secret: fakeModelSecret, Vendor: "anthropic", Kind: "subscription", Plan: "max", AccountRef: "acct-fake"}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/credentials/for-job", l.modelForJob)
	mux.HandleFunc("/oauth/session-token/request", l.sessionToken)
	mux.HandleFunc("POST /v1/credentials/for-job/source", l.source)
	mux.HandleFunc("POST /v1/credentials/for-job/release", l.release)
	mux.HandleFunc("DELETE /v1/credentials/for-job/source", l.source)
	mux.HandleFunc("/github/{path...}", l.proxy)
	mux.HandleFunc("/v1/connections/for-job/{binding}/mcp", l.slot)
	mux.HandleFunc("POST /v1/federation/token", l.federationToken)
	l.srv = httptest.NewServer(mux)
	t.Cleanup(l.srv.Close)
	return l
}

// open is Mesh, as attester, opening the lease's session for jkt.
func (l *fakeLatchkey) open(leaseID, jkt string) (*meshapi.SessionCredential, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	sid := fmt.Sprintf("00000000-0000-4000-8000-%012d", l.n)
	cred := fmt.Sprintf("lksr_%043d", l.n)
	l.jkt[sid], l.creds[cred], l.leaseOf[sid] = jkt, sid, leaseID
	return &meshapi.SessionCredential{
		SessionID: sid, RequestURL: l.srv.URL + "/oauth/session-token/request?session=" + sid + "&v=1",
		RequestToken: cred, ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	}, http.StatusCreated
}

func (l *fakeLatchkey) fail(w http.ResponseWriter, status int, msg string) {
	l.mu.Lock()
	l.failures = append(l.failures, msg)
	l.mu.Unlock()
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": "invalid", "error_description": msg})
}

func (l *fakeLatchkey) sessionToken(w http.ResponseWriter, r *http.Request) {
	cred, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	l.mu.Lock()
	sid, ok := l.creds[cred]
	jkt := l.jkt[sid]
	l.mu.Unlock()
	aud := r.URL.Query().Get("audience")
	if !ok || r.URL.Query().Get("session") != sid || (aud != MeshSessionAudience && aud != SigstoreAudience) {
		l.fail(w, http.StatusUnauthorized, "request token not valid for this session")
		return
	}
	if aud == SigstoreAudience {
		// Fulcio's ID token: never DPoP-bound, a proof not read (Latchkey
		// ADR 017 decision 3), answered without token_type
		claims, _ := json.Marshal(map[string]any{"sub": "mesh:acme/conform:job:" + sid + ":lease:1", "aud": SigstoreAudience, "iss": l.srv.URL})
		l.mu.Lock()
		l.signingMints++
		l.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"value": "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".c2ln", "expires_in": 600})
		return
	}
	if err := l.v.Verify(r.Header.Get("DPoP"), dpoptest.Request{Method: r.Method, URL: l.srv.URL + r.URL.Path, JKT: jkt}); err != nil {
		l.fail(w, http.StatusBadRequest, "session token proof: "+err.Error())
		return
	}
	l.mu.Lock()
	l.n++
	tok := fmt.Sprintf("mst_%d", l.n)
	l.tokens[tok] = jkt
	l.leaseOf[tok] = l.leaseOf[sid]
	l.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"value": tok, "expires_in": 600, "token_type": "DPoP"})
}

// checkToken checks a DPoP-bound session token and its proof for url.
func (l *fakeLatchkey) checkToken(r *http.Request, url string) (string, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "DPoP ")
	l.mu.Lock()
	jkt, known := l.tokens[tok]
	l.mu.Unlock()
	if !ok || !known {
		return "", fmt.Errorf("not a session token: %q", r.Header.Get("Authorization"))
	}
	if err := l.v.Verify(r.Header.Get("DPoP"), dpoptest.Request{Method: r.Method, URL: url, Token: tok, JKT: jkt}); err != nil {
		return "", err
	}
	return tok, nil
}

// modelForJob answers the job's model credential on the session token,
// as Latchkey's for-job route does.
func (l *fakeLatchkey) modelForJob(w http.ResponseWriter, r *http.Request) {
	if _, err := l.checkToken(r, l.srv.URL+r.URL.Path); err != nil {
		l.fail(w, http.StatusUnauthorized, "for-job: "+err.Error())
		return
	}
	// a body naming a credential_ref asks for a carried credential, as
	// latchkey #694's connectionForJob (and ADR 031 §2.4 for a Secret)
	var body struct {
		CredentialRef string `json:"credential_ref"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if ref := body.CredentialRef; ref != "" {
		l.mu.Lock()
		l.asked = append(l.asked, ref)
		c, ok := l.carried[ref]
		rf, refused := l.refuseCarried[ref]
		l.mu.Unlock()
		switch {
		case refused:
			w.WriteHeader(rf.status)
			json.NewEncoder(w).Encode(map[string]string{"error": rf.code, "error_description": "refused by the fake"})
		case !ok:
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"error": "not_the_jobs", "error_description": "the ref is not on the job's access"})
		default:
			w.Header().Set("Cache-Control", "no-store")
			json.NewEncoder(w).Encode(c)
		}
		return
	}
	l.mu.Lock()
	l.modelReads++
	cred, refuse := l.model, l.refuseModel
	l.mu.Unlock()
	if refuse != "" {
		l.fail(w, http.StatusForbidden, refuse)
		return
	}
	cred.ExpiresAt = time.Now().Add(10 * time.Minute).UTC()
	json.NewEncoder(w).Encode(cred)
}

func (l *fakeLatchkey) source(w http.ResponseWriter, r *http.Request) {
	if _, err := l.checkToken(r, l.srv.URL+r.URL.Path); err != nil {
		l.fail(w, http.StatusUnauthorized, "source: "+err.Error())
		return
	}
	var body struct {
		Vendor     string `json:"vendor"`
		BaseURL    string `json:"base_url"`
		Repository string `json:"repository"`
		Access     string `json:"access"`
		Token      string `json:"token"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	l.mu.Lock()
	refuse := l.refuseSource
	l.mu.Unlock()
	if r.Method == http.MethodPost && refuse != "" {
		l.fail(w, http.StatusServiceUnavailable, refuse)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.Method == http.MethodPost {
		l.mints = append(l.mints, SourceRequest{Vendor: body.Vendor, BaseURL: body.BaseURL, Repository: body.Repository, Access: body.Access})
	}
	if r.Method == http.MethodDelete {
		l.revoked = append(l.revoked, body.Token)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	l.n++
	tok := fmt.Sprintf("ghs_%d", l.n)
	l.minted = append(l.minted, tok)
	l.access = append(l.access, body.Access)
	json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_at": time.Now().Add(time.Hour).UTC(), "repository": body.Repository, "access": body.Access})
}

// proxy is Latchkey's vendor API proxy (L4) as the relay reaches it:
// the session token and a proof of the call, answered with what the
// call was.
func (l *fakeLatchkey) proxy(w http.ResponseWriter, r *http.Request) {
	if _, err := l.checkToken(r, l.srv.URL+r.URL.Path); err != nil {
		l.fail(w, http.StatusUnauthorized, "proxy: "+err.Error())
		return
	}
	l.mu.Lock()
	l.api = append(l.api, r.Method+" github/"+r.PathValue("path"))
	l.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"number": 1})
}

// apiCalls are the proxied calls, in order.
func (l *fakeLatchkey) apiCalls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.api...)
}

// accesses are each mint's access, in order.
func (l *fakeLatchkey) accesses() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.access...)
}

func (l *fakeLatchkey) snapshot() (minted, revoked, failures []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.minted...), append([]string(nil), l.revoked...), append([]string(nil), l.failures...)
}

// fakeGitHub is git http-backend over a bare acme/fixture with one
// commit, recording each request's Authorization.
type fakeGitHub struct {
	srv  *httptest.Server
	head string
	bare string
	mu   sync.Mutex
	auth []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path: " + err.Error())
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("no git-http-backend")
	}
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	served := filepath.Join(root, "served")
	bare := filepath.Join(served, "acme", "fixture.git")
	os.MkdirAll(filepath.Dir(bare), 0o755)
	git(root, "init", "-q", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	git(root, "init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("the fixture\n"), 0o644)
	git(seed, "add", ".")
	git(seed, "commit", "-qm", "one")
	git(seed, "push", "-q", bare, "main")
	g := &fakeGitHub{head: git(seed, "rev-parse", "HEAD"), bare: bare}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		g.mu.Unlock()
		(&cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + served, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x-access-token"}}).ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// ref is the sha ref names in the fixture, "" when it names none.
func (g *fakeGitHub) ref(name string) string {
	out, err := exec.Command("git", "-C", g.bare, "rev-parse", "-q", "--verify", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (g *fakeGitHub) auths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.auth...)
}

// product is a product's MCP server, as meshconform's: it takes only a
// mesh-session token with a DPoP proof signed for its own URL, records
// each call, and answers submit with the job's result.
type product struct {
	l   *fakeLatchkey
	url string

	mu       sync.Mutex
	calls    []string
	failures []string
	// learned are the learnings each call of learn carried
	learned []string
}

func (p *product) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, err := p.l.checkToken(r, p.url); err != nil {
		p.mu.Lock()
		p.failures = append(p.failures, err.Error())
		p.mu.Unlock()
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	b, _ := io.ReadAll(r.Body)
	var msg rpcCall
	json.Unmarshal(b, &msg)
	p.mu.Lock()
	p.calls = append(p.calls, msg.Method+" "+msg.Params.Name)
	if msg.Params.Name == "learn" {
		p.learned = append(p.learned, string(msg.Params.Arguments.Learnings))
	}
	p.mu.Unlock()
	if msg.Method == "tools/list" {
		// the conform product's tools, and one no kind names
		var tools []map[string]any
		for _, n := range []string{"brief", "submit", "learn", "admin_delete"} {
			tools = append(tools, map[string]any{"name": n, "description": "the product's own " + n, "inputSchema": map[string]any{"type": "object"}})
		}
		ans, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"tools": tools}})
		w.Header().Set("Content-Type", "application/json")
		w.Write(ans)
		return
	}
	text := "ok"
	if msg.Params.Name == "submit" {
		text = `{"sha256":"5f0c"}`
	}
	ans, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}})
	w.Header().Set("Content-Type", "application/json")
	w.Write(ans)
}

func (p *product) snapshot() (calls, failures []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...), append([]string(nil), p.failures...)
}

// release is latchkey #694's for-job release: every child minted on the
// session revoked, answered {released: n}.
func (l *fakeLatchkey) release(w http.ResponseWriter, r *http.Request) {
	if _, err := l.checkToken(r, l.srv.URL+r.URL.Path); err != nil {
		l.fail(w, http.StatusUnauthorized, "release: "+err.Error())
		return
	}
	l.mu.Lock()
	l.releases++
	refuse, hook := l.refuseRelease, l.onRelease
	minted := 0
	for _, a := range l.asked {
		if strings.HasPrefix(a, "conn_") {
			minted++
		}
	}
	l.mu.Unlock()
	if hook != nil {
		hook()
	}
	if refuse != 0 {
		w.WriteHeader(refuse)
		json.NewEncoder(w).Encode(map[string]string{"error": "temporarily_unavailable", "error_description": "refused by the fake"})
		return
	}
	json.NewEncoder(w).Encode(map[string]int{"released": minted})
}
