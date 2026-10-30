package gitrelay

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// startServer is a started Server in front of h.
func startServer(t *testing.T, h http.Handler) (*Server, *[]Event, *sync.Mutex) {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var evs []Event
	var mu sync.Mutex
	s := &Server{Upstream: up.URL, Audit: func(e Event) { mu.Lock(); evs = append(evs, e); mu.Unlock() }}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return s, &evs, &mu
}

// minter counts its mints and answers tok-<n>, living for ttl.
type minter struct {
	mu  sync.Mutex
	n   int
	ttl time.Duration
}

func (m *minter) mint(context.Context, string, Access) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	return Token{Value: fmt.Sprintf("tok-%d", m.n), ExpiresAt: time.Now().Add(m.ttl)}, nil
}

// revoker records each Revoke call's tokens.
type revoker struct {
	mu    sync.Mutex
	calls [][]Token
}

func (r *revoker) revoke(_ context.Context, minted []Token) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, minted)
	return nil
}

func status(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func basic(tok string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))
}

// A lease's grant keeps one capability for its life, whatever its token
// does: the same lease again answers the same grant, another policy for
// it is refused, and each request is audited under the lease.
func TestGrantCapabilityIsKeyedByTheLease(t *testing.T) {
	gh := &fakeGitHub{}
	s, evs, mu := startServer(t, gh)
	m := &minter{ttl: time.Hour}
	p := Policy{Repo: "acme/app", Access: Read}
	g, err := s.Open("lease-1", p, m.mint, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Open("lease-1", p, m.mint, nil)
	if err != nil || again != g {
		t.Fatalf("the same lease again: %v, %v", again, err)
	}
	if _, err := s.Open("lease-1", Policy{Repo: "acme/other", Access: Read}, m.mint, nil); err == nil {
		t.Fatal("another policy for the same lease was accepted")
	}
	other, _ := s.Open("lease-2", p, m.mint, nil)
	if other.Base() == g.Base() || !strings.HasPrefix(g.Base(), "http://127.0.0.1:") || !strings.HasSuffix(g.Base(), "/j/"+g.capability+"/git/") {
		t.Fatalf("bases %s %s", g.Base(), other.Base())
	}
	if c := status(t, g.Base()+"acme/app.git/info/refs?service=git-upload-pack"); c != 200 {
		t.Fatalf("own repo: %d", c)
	}
	if c := status(t, g.Base()+"acme/other.git/info/refs?service=git-upload-pack"); c != http.StatusForbidden {
		t.Fatalf("another repo: %d", c)
	}
	if gh.auth[0] != basic("tok-1") {
		t.Fatalf("upstream auth %q", gh.auth[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*evs) != 2 || (*evs)[0].Lease != "lease-1" || (*evs)[1].Lease != "lease-1" || (*evs)[1].Outcome != "refused" {
		t.Fatalf("audit %+v", *evs)
	}
	if got := s.Grant("lease-2"); got != other {
		t.Fatalf("Grant(lease-2) = %v", got)
	}
	if len(s.Leases()) != 2 {
		t.Fatalf("leases %v", s.Leases())
	}
}

// The relay refreshes the grant's token through Mint as it nears
// expiry, the capability unchanged.
func TestGrantRefreshesItsTokenThroughMint(t *testing.T) {
	gh := &fakeGitHub{}
	s, _, _ := startServer(t, gh)
	// a token inside the refresh window is replaced on every request
	m := &minter{ttl: time.Minute}
	g, err := s.Open("lease-1", Policy{Repo: "acme/app", Access: Read}, m.mint, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := g.Base()
	for range 2 {
		if c := status(t, base+"acme/app.git/info/refs?service=git-upload-pack"); c != 200 {
			t.Fatalf("fetch: %d", c)
		}
	}
	if g.Base() != base {
		t.Fatalf("the capability moved: %s → %s", base, g.Base())
	}
	if gh.auth[0] != basic("tok-1") || gh.auth[1] != basic("tok-2") {
		t.Fatalf("upstream auth %q", gh.auth)
	}
}

// Close removes the capability (404 on the loopback URL and on the
// session's route alike) and calls Revoke once with every token the
// grant minted; a second Close does nothing.
func TestGrantCloseRevokesOnceAndAnswers404(t *testing.T) {
	gh := &fakeGitHub{}
	s, _, _ := startServer(t, gh)
	m := &minter{ttl: time.Minute}
	rv := &revoker{}
	g, err := s.Open("lease-1", Policy{Repo: "acme/app", Access: Read}, m.mint, rv.revoke)
	if err != nil {
		t.Fatal(err)
	}
	base := g.Base()
	status(t, base+"acme/app.git/info/refs?service=git-upload-pack")
	rec := do(g, http.MethodGet, "/git/acme/app.git/info/refs?service=git-upload-pack", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("the session's route: %d", rec.Code)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if c := status(t, base+"acme/app.git/info/refs?service=git-upload-pack"); c != http.StatusNotFound {
		t.Fatalf("after close: %d", c)
	}
	if rec := do(g, http.MethodGet, "/git/acme/app.git/info/refs?service=git-upload-pack", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("the session's route after close: %d", rec.Code)
	}
	if len(rv.calls) != 1 || len(rv.calls[0]) != 2 || rv.calls[0][0].Value != "tok-1" || rv.calls[0][1].Value != "tok-2" {
		t.Fatalf("revoke calls %+v", rv.calls)
	}
	if !g.Closed() || s.Grant("lease-1") != nil || len(s.Leases()) != 0 {
		t.Fatal("the grant is still listed")
	}
	// the lease opens afresh after a close: a new capability
	g2, err := s.Open("lease-1", Policy{Repo: "acme/app", Access: Read}, m.mint, rv.revoke)
	if err != nil || g2 == g || g2.Base() == base {
		t.Fatalf("reopen: %v %v", g2, err)
	}
}

// Close cuts a stream in flight before it revokes: a clone mid-pack ends
// and Revoke runs after the stream has returned.
func TestGrantCloseCutsStreamsInFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, "first-bytes")
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	s, _, _ := startServer(t, up)
	var order []string
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	g, err := s.Open("lease-1", Policy{Repo: "acme/app", Access: Read}, (&minter{ttl: time.Hour}).mint,
		func(context.Context, []Token) error { note("revoke"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(g.Base()+"acme/app.git/git-upload-pack", "application/x-git-upload-pack-request", strings.NewReader("0000"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	read := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		note("stream ended")
		read <- err
	}()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived the grant")
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if !contains(order, "revoke") {
		t.Fatalf("no revoke: %v", order)
	}
}

// A grant and a token-keyed registration share the server: the legacy
// node's Register keeps working beside v1 leases.
func TestRegisterKeepsWorkingBesideGrants(t *testing.T) {
	gh := &fakeGitHub{}
	s, _, _ := startServer(t, gh)
	g, err := s.Open("lease-1", Policy{Repo: "acme/app", Access: Read}, (&minter{ttl: time.Hour}).mint, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.Register(Policy{Repo: "acme/legacy", Access: Read}, Token{Value: "legacy", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if c := status(t, base+"acme/legacy.git/info/refs?service=git-upload-pack"); c != 200 {
		t.Fatalf("legacy: %d", c)
	}
	if c := status(t, g.Base()+"acme/app.git/info/refs?service=git-upload-pack"); c != 200 {
		t.Fatalf("grant: %d", c)
	}
	if gh.auth[0] != basic("legacy") || gh.auth[1] != basic("tok-1") {
		t.Fatalf("upstream auth %q", gh.auth)
	}
	if len(s.Leases()) != 1 {
		t.Fatalf("leases %v", s.Leases())
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
