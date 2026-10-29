package nodev1

import (
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
	minted   []string
	revoked  []string
	failures []string
	n        int
}

func newFakeLatchkey(t *testing.T) *fakeLatchkey {
	l := &fakeLatchkey{jkt: map[string]string{}, creds: map[string]string{}, tokens: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/session-token/request", l.sessionToken)
	mux.HandleFunc("POST /v1/credentials/for-job/source", l.source)
	mux.HandleFunc("DELETE /v1/credentials/for-job/source", l.source)
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
	l.jkt[sid], l.creds[cred] = jkt, sid
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
	if !ok || r.URL.Query().Get("session") != sid || r.URL.Query().Get("audience") != MeshSessionAudience {
		l.fail(w, http.StatusUnauthorized, "request token not valid for this session")
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

func (l *fakeLatchkey) source(w http.ResponseWriter, r *http.Request) {
	if _, err := l.checkToken(r, l.srv.URL+r.URL.Path); err != nil {
		l.fail(w, http.StatusUnauthorized, "source: "+err.Error())
		return
	}
	var body struct{ Repository, Access, Token string }
	json.NewDecoder(r.Body).Decode(&body)
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.Method == http.MethodDelete {
		l.revoked = append(l.revoked, body.Token)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	l.n++
	tok := fmt.Sprintf("ghs_%d", l.n)
	l.minted = append(l.minted, tok)
	json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_at": time.Now().Add(time.Hour).UTC(), "repository": body.Repository, "access": body.Access})
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
	g := &fakeGitHub{head: git(seed, "rev-parse", "HEAD")}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		g.mu.Unlock()
		(&cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + served, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=x-access-token"}}).ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
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
	p.mu.Unlock()
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
