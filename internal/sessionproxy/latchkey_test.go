package sessionproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
)

const sessionTok = "mesh-session-token-abc"

// fakeLatchkeyProxy is Latchkey's API proxy as the relay reaches it: it
// takes `Authorization: DPoP <session token>` with a proof of the call
// by the session's key, and answers what its handler says.
type fakeLatchkeyProxy struct {
	t      *testing.T
	key    *dpop.Key
	v      dpoptest.Verifier
	nonce  string
	answer func(w http.ResponseWriter, r *http.Request, body string)

	mu    sync.Mutex
	calls []*http.Request
	bad   []string
}

func (f *fakeLatchkeyProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, r)
	f.mu.Unlock()
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "DPoP ")
	if !ok || tok != sessionTok {
		f.fail("authorization " + r.Header.Get("Authorization"))
		w.Header().Set("X-Latchkey-Error", "invalid_token")
		http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
		return
	}
	if f.nonce != "" {
		if n, _ := dpoptest.ProofNonce(r.Header.Get("DPoP")); n != f.nonce {
			w.Header().Set("DPoP-Nonce", f.nonce)
			http.Error(w, `{"error":"use_dpop_nonce"}`, http.StatusUnauthorized)
			return
		}
	}
	if err := f.v.Verify(r.Header.Get("DPoP"), dpoptest.Request{Method: r.Method, URL: "http://" + r.Host + r.URL.RequestURI(), Token: tok, JKT: f.key.Thumbprint()}); err != nil {
		f.fail("proof: " + err.Error())
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	f.answer(w, r, string(b))
}

func (f *fakeLatchkeyProxy) fail(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bad = append(f.bad, s)
}

func (f *fakeLatchkeyProxy) seen() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.calls...)
}

// latchkeyRig is a proxy whose /latchkey/ route reaches a fake Latchkey,
// allowing GitHub pulls and a GitLab merge request read, and hearing
// every call.
type latchkeyRig struct {
	lk    *fakeLatchkeyProxy
	up    *httptest.Server
	c     *http.Client
	mu    sync.Mutex
	audit []APICall
}

func newLatchkeyRig(t *testing.T, answer func(w http.ResponseWriter, r *http.Request, body string)) *latchkeyRig {
	t.Helper()
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	g := &latchkeyRig{lk: &fakeLatchkeyProxy{t: t, key: key, answer: answer}}
	g.up = httptest.NewServer(g.lk)
	t.Cleanup(g.up.Close)
	p := New(nil)
	p.Latchkey = &Latchkey{
		URL:   g.up.URL,
		Token: func(context.Context) (string, error) { return sessionTok, nil },
		Key:   key,
		Allow: func(vendor, method, path string) bool {
			switch vendor + " " + method {
			case "github POST", "github GET":
				return strings.HasPrefix(path, "repos/acme/app/pulls")
			case "gitlab GET":
				return strings.HasPrefix(path, "api/v4/projects/")
			}
			return false
		},
		Audit: func(c APICall) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.audit = append(g.audit, c)
		},
	}
	g.c, _ = listen(t, p)
	return g
}

func (g *latchkeyRig) call(t *testing.T, method, path, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, "http://relay"+path, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := g.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (g *latchkeyRig) heard() []APICall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]APICall(nil), g.audit...)
}

// A vendor API write goes through the relay (Mesh v1 wave 2.7): the
// session's call of /latchkey/github/<path> reaches Latchkey at
// /github/<path> with the lease's session token as `DPoP` and a fresh
// proof of the call, its query and body as sent; the session's own
// Authorization never leaves the node, and the session token never
// comes back. Each call is one APICall naming vendor, method and path.
func TestLatchkeyRouteRelaysOnTheSessionToken(t *testing.T) {
	g := newLatchkeyRig(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("Set-Cookie", "session=leak")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"number":7,"echo":`+body+`,"auth":"`+sessionTok+`"}`)
	})
	resp, body := g.call(t, http.MethodPost, "/latchkey/github/repos/acme/app/pulls?draft=1", `{"title":"t"}`,
		map[string]string{"Authorization": "Bearer session-own", "Content-Type": "application/json", "Cookie": "a=b"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, sessionTok) || !strings.Contains(body, `"echo":{"title":"t"}`) {
		t.Fatalf("answer not relayed masked: %s", body)
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "4999" || resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("headers relayed: %v", resp.Header)
	}
	if len(g.lk.bad) > 0 {
		t.Fatalf("Latchkey refused: %v", g.lk.bad)
	}
	seen := g.lk.seen()
	if len(seen) != 1 {
		t.Fatalf("Latchkey saw %d calls", len(seen))
	}
	r := seen[0]
	if r.URL.Path != "/github/repos/acme/app/pulls" || r.URL.RawQuery != "draft=1" || r.Header.Get("Cookie") != "" {
		t.Fatalf("Latchkey saw %s?%s cookie %q", r.URL.Path, r.URL.RawQuery, r.Header.Get("Cookie"))
	}
	a := g.heard()
	if len(a) != 1 || a[0].Vendor != "github" || a[0].Method != "POST" || a[0].Path != "repos/acme/app/pulls" || a[0].Outcome != "ok" || a[0].Status != 201 || a[0].Bytes == 0 {
		t.Fatalf("audit %+v", a)
	}
}

// The node holds a call to the job's access.api before anything leaves
// it: a call no rule allows, a bad vendor, method or path is refused
// with the relay's own error, never reaches Latchkey, and is heard as
// refused.
func TestLatchkeyRouteRefusesBeforeLeaving(t *testing.T) {
	g := newLatchkeyRig(t, func(w http.ResponseWriter, r *http.Request, body string) {
		t.Errorf("Latchkey reached: %s %s", r.Method, r.URL.Path)
	})
	cases := []struct {
		name, method, path string
		status             int
		code               string
	}{
		{"no rule", http.MethodPut, "/latchkey/github/repos/acme/app/pulls/1/merge", 403, "policy_denied"},
		{"another repository", http.MethodPost, "/latchkey/github/repos/other/app/pulls", 403, "policy_denied"},
		{"vendor no rule names", http.MethodGet, "/latchkey/bitbucket/2.0/repositories/acme/app", 403, "policy_denied"},
		{"bad vendor", http.MethodGet, "/latchkey/Git%20Hub/repos", 404, "bad_vendor"},
		{"bad method", "PROPFIND", "/latchkey/github/repos/acme/app/pulls", 405, "bad_method"},
		{"traversal", http.MethodGet, "/latchkey/github/repos/acme/app/pulls/%2e%2e/x", 400, "bad_path"},
		{"empty path", http.MethodGet, "/latchkey/github/", 400, "bad_path"},
		{"encoded dot segment", http.MethodGet, "/latchkey/github/repos/acme/app/pulls/%2E%2E", 400, "bad_path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := g.call(t, c.method, c.path, "", nil)
			if resp.StatusCode != c.status || resp.Header.Get(RelayErrorHeader) != c.code {
				t.Fatalf("got %d %s %s", resp.StatusCode, resp.Header.Get(RelayErrorHeader), body)
			}
		})
	}
	a := g.heard()
	if len(a) != len(cases) {
		t.Fatalf("heard %d of %d refusals", len(a), len(cases))
	}
	for _, c := range a {
		if c.Outcome != "refused" || c.Reason == "" || c.Status != 0 {
			t.Fatalf("refusal heard as %+v", c)
		}
	}
}

// Latchkey's own refusal (X-Latchkey-Error) is relayed as Latchkey
// answered and heard as refused; a vendor's error status is heard as
// failed; a 304 is ok.
func TestLatchkeyRouteHearsLatchkeyAndVendorRefusals(t *testing.T) {
	g := newLatchkeyRig(t, func(w http.ResponseWriter, r *http.Request, body string) {
		switch {
		case r.URL.Query().Get("as") == "denied":
			w.Header().Set("X-Latchkey-Error", "policy_denied")
			w.WriteHeader(http.StatusForbidden)
		case r.URL.Query().Get("as") == "cached":
			w.WriteHeader(http.StatusNotModified)
		default:
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
	})
	for _, c := range []struct {
		query, outcome string
		status         int
	}{{"denied", "refused", 403}, {"cached", "ok", 304}, {"bad", "failed", 422}} {
		resp, _ := g.call(t, http.MethodGet, "/latchkey/github/repos/acme/app/pulls?as="+c.query, "", nil)
		if resp.StatusCode != c.status {
			t.Fatalf("%s: status %d", c.query, resp.StatusCode)
		}
	}
	a := g.heard()
	if len(a) != 3 || a[0].Outcome != "refused" || a[0].Reason != "latchkey: policy_denied" || a[1].Outcome != "ok" || a[2].Outcome != "failed" {
		t.Fatalf("heard %+v", a)
	}
}

// Latchkey's DPoP nonce is answered: the call is asked once more with a
// proof carrying it, its body sent again.
func TestLatchkeyRouteAnswersTheNonce(t *testing.T) {
	g := newLatchkeyRig(t, func(w http.ResponseWriter, r *http.Request, body string) {
		io.WriteString(w, body)
	})
	g.lk.nonce = "n-1"
	resp, body := g.call(t, http.MethodPost, "/latchkey/github/repos/acme/app/pulls", `{"title":"again"}`, nil)
	if resp.StatusCode != 200 || body != `{"title":"again"}` {
		t.Fatalf("status %d %s (%v)", resp.StatusCode, body, g.lk.bad)
	}
	if n := len(g.lk.seen()); n != 2 {
		t.Fatalf("Latchkey saw %d calls, want the first and the nonce's", n)
	}
}

// Latchkey's Link points pagination at itself; the relay points it back
// through the route, so the session pages through the relay too.
func TestLatchkeyRouteRewritesLink(t *testing.T) {
	var base string
	g := newLatchkeyRig(t, func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Link", `<`+base+`/gitlab/api/v4/projects/1/merge_requests?page=2>; rel="next"`)
		io.WriteString(w, "[]")
	})
	base = g.up.URL
	resp, _ := g.call(t, http.MethodGet, "/latchkey/gitlab/api/v4/projects/1/merge_requests", "", nil)
	if got := resp.Header.Get("Link"); got != `<http://relay/latchkey/gitlab/api/v4/projects/1/merge_requests?page=2>; rel="next"` {
		t.Fatalf("Link %q", got)
	}
}

// A session whose lease names no vendor API reaches none.
func TestNoLatchkeyRouteWithoutAPIAccess(t *testing.T) {
	p := New(nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/latchkey/github/repos/acme/app", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get(RelayErrorHeader) != "no_api" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get(RelayErrorHeader))
	}
}

// An escaped segment goes on escaped: GitLab's project path is one
// segment, never decoded into more.
func TestLatchkeyRouteKeepsEscapedSegments(t *testing.T) {
	g := newLatchkeyRig(t, func(w http.ResponseWriter, r *http.Request, body string) { io.WriteString(w, "{}") })
	resp, _ := g.call(t, http.MethodGet, "/latchkey/gitlab/api/v4/projects/acme%2Fapp", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d (%v)", resp.StatusCode, g.lk.bad)
	}
	if got := g.lk.seen()[0].URL.EscapedPath(); got != "/gitlab/api/v4/projects/acme%2Fapp" {
		t.Fatalf("Latchkey saw %s", got)
	}
}
