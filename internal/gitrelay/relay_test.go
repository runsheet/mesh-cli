package gitrelay

import (
	"bytes"
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

// fakeGitHub records what reached it and answers 200 with a fixed body.
type fakeGitHub struct {
	mu   sync.Mutex
	reqs []*http.Request
	auth []string
	body [][]byte
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.body = append(f.body, b)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Set-Cookie", "never=returned")
	io.WriteString(w, "pack-bytes")
}

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

func pushBody(cmds ...string) []byte {
	var b strings.Builder
	for i, c := range cmds {
		if i == 0 {
			c += "\x00report-status side-band-64k"
		}
		b.WriteString(pkt(c + "\n"))
	}
	b.WriteString("0000")
	b.WriteString("PACKdata")
	return []byte(b.String())
}

func relayFor(t *testing.T, p Policy) (*Relay, *fakeGitHub, *[]Event) {
	t.Helper()
	gh := &fakeGitHub{}
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	var evs []Event
	var mu sync.Mutex
	mints := 0
	r := &Relay{
		Policy:   p,
		Upstream: srv.URL,
		Mint: func(context.Context) (Token, error) {
			mints++
			return Token{Value: fmt.Sprintf("tok-%d", mints), ExpiresAt: time.Now().Add(time.Hour)}, nil
		},
		Audit: func(e Event) { mu.Lock(); evs = append(evs, e); mu.Unlock() },
	}
	return r, gh, &evs
}

func do(r http.Handler, method, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestRelayFetchAddsTheTokenAndNothingElse(t *testing.T) {
	r, gh, evs := relayFor(t, Policy{Repo: "acme/app", Access: Read})
	rec := do(r, "GET", "/git/acme/app.git/info/refs?service=git-upload-pack", nil, map[string]string{
		"Authorization": "Basic c2Vzc2lvbjpzZWNyZXQ=", "Cookie": "x=y", "Git-Protocol": "version=2",
	})
	if rec.Code != 200 || rec.Body.String() != "pack-bytes" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("an upstream header outside the list came back")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok-1"))
	if gh.auth[0] != want {
		t.Fatalf("upstream auth %q, want the job's token", gh.auth[0])
	}
	if got := gh.reqs[0].Header.Get("Git-Protocol"); got != "version=2" {
		t.Fatalf("Git-Protocol not forwarded: %q", got)
	}
	if gh.reqs[0].Header.Get("Cookie") != "" {
		t.Fatal("a client cookie reached GitHub")
	}
	if gh.reqs[0].URL.Path != "/acme/app.git/info/refs" || gh.reqs[0].URL.RawQuery != "service=git-upload-pack" {
		t.Fatalf("upstream path %s?%s", gh.reqs[0].URL.Path, gh.reqs[0].URL.RawQuery)
	}
	if len(*evs) != 1 || (*evs)[0].Outcome != "ok" || (*evs)[0].Service != "upload-pack" || (*evs)[0].Bytes != int64(len("pack-bytes")) {
		t.Fatalf("audit %+v", *evs)
	}
}

func TestRelayRefusesAnotherRepository(t *testing.T) {
	r, gh, evs := relayFor(t, Policy{Repo: "acme/app", Access: Write, PushRef: "refs/heads/mesh/g-1"})
	rec := do(r, "GET", "/git/acme/other.git/info/refs?service=git-upload-pack", nil, nil)
	if rec.Code != http.StatusForbidden || len(gh.reqs) != 0 {
		t.Fatalf("got %d, %d upstream requests", rec.Code, len(gh.reqs))
	}
	if (*evs)[0].Outcome != "refused" {
		t.Fatalf("audit %+v", *evs)
	}
	if rec := do(r, "GET", "/git/acme/app/info/refs?service=git-upload-pack", nil, nil); rec.Code != 200 {
		t.Fatalf("a remote named without .git: got %d", rec.Code)
	}
	for _, p := range []string{"/git/../app.git/info/refs", "/git/acme/..", "/git/acme", "/mcp/x"} {
		if rec := do(r, "GET", p+"?service=git-upload-pack", nil, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: got %d", p, rec.Code)
		}
	}
}

func TestRelayReadJobCannotPush(t *testing.T) {
	r, gh, _ := relayFor(t, Policy{Repo: "acme/app", Access: Read})
	if rec := do(r, "GET", "/git/acme/app.git/info/refs?service=git-receive-pack", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("advertisement: got %d", rec.Code)
	}
	body := pushBody(zeroID + " 1111111111111111111111111111111111111111 refs/heads/mesh/g-1")
	if rec := do(r, "POST", "/git/acme/app.git/git-receive-pack", body, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("push: got %d", rec.Code)
	}
	if len(gh.reqs) != 0 {
		t.Fatal("a refused push reached GitHub")
	}
}

func TestRelayPushOnlyItsRef(t *testing.T) {
	p := Policy{Repo: "acme/app", Access: Write, PushRef: "refs/heads/mesh/g-1"}
	r, gh, evs := relayFor(t, p)
	ok := pushBody("2222222222222222222222222222222222222222 1111111111111111111111111111111111111111 refs/heads/mesh/g-1")
	if rec := do(r, "POST", "/git/acme/app.git/git-receive-pack", ok, nil); rec.Code != 200 {
		t.Fatalf("own ref: got %d %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(gh.body[0], ok) {
		t.Fatal("the push body was not replayed byte for byte")
	}
	pinned := pushBody("2222222222222222222222222222222222222222 1111111111111111111111111111111111111111 refs/heads/mesh/g-1",
		zeroID+" 1111111111111111111111111111111111111111 refs/runsheet/changesets/7f4e0282-e04a-5639-ad32-d58275988f03")
	if rec := do(r, "POST", "/git/acme/app.git/git-receive-pack", pinned, nil); rec.Code != 200 {
		t.Fatalf("branch and pinned changeset ref: got %d %s", rec.Code, rec.Body.String())
	}
	for name, body := range map[string][]byte{
		"main":       pushBody("2222222222222222222222222222222222222222 1111111111111111111111111111111111111111 refs/heads/main"),
		"second ref": pushBody("2222222222222222222222222222222222222222 1111111111111111111111111111111111111111 refs/heads/mesh/g-1", zeroID+" 1111111111111111111111111111111111111111 refs/tags/v9"),
		"delete":     pushBody("2222222222222222222222222222222222222222 " + zeroID + " refs/heads/mesh/g-1"),
		"no command": []byte("0000PACK"),
		"compressed": ok,
	} {
		hdr := map[string]string(nil)
		if name == "compressed" {
			hdr = map[string]string{"Content-Encoding": "gzip"}
		}
		if rec := do(r, "POST", "/git/acme/app.git/git-receive-pack", body, hdr); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: got %d", name, rec.Code)
		}
	}
	if len(gh.reqs) != 2 {
		t.Fatalf("%d pushes reached GitHub, want 2", len(gh.reqs))
	}
	if last := (*evs)[len(*evs)-1]; last.Outcome != "refused" {
		t.Fatalf("last audit %+v", last)
	}
}

func TestRelayCachesTheTokenUntilNearExpiry(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	mints := 0
	gh := &fakeGitHub{}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	r := &Relay{
		Policy:   Policy{Repo: "acme/app", Access: Read},
		Upstream: srv.URL,
		Now:      func() time.Time { return now },
		Mint: func(context.Context) (Token, error) {
			mints++
			return Token{Value: fmt.Sprintf("tok-%d", mints), ExpiresAt: now.Add(time.Hour)}, nil
		},
	}
	get := func() { do(r, "GET", "/git/acme/app.git/info/refs?service=git-upload-pack", nil, nil) }
	get()
	now = now.Add(45 * time.Minute)
	get()
	if mints != 1 {
		t.Fatalf("minted %d times within the token's life", mints)
	}
	now = now.Add(6 * time.Minute) // 9 minutes left: inside the refresh window
	get()
	if mints != 2 {
		t.Fatalf("minted %d times, want a fresh token ten minutes before expiry", mints)
	}
}

func TestRelayNeverEchoesTheToken(t *testing.T) {
	r := &Relay{
		Policy:   Policy{Repo: "acme/app", Access: Read},
		Upstream: "http://127.0.0.1:1",
		Mint: func(context.Context) (Token, error) {
			return Token{Value: "s3cret", ExpiresAt: time.Now().Add(time.Hour)}, nil
		},
	}
	rec := do(r, "GET", "/git/acme/app.git/info/refs?service=git-upload-pack", nil, nil)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}
