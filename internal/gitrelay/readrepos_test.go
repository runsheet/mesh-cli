package gitrelay

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mintLog records each mint's repository and access and answers
// tok-<repo>-<n>.
type mintLog struct {
	mu    sync.Mutex
	calls []string
}

func (m *mintLog) mint(_ context.Context, repo string, access Access) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, repo+" "+string(access))
	return Token{Value: fmt.Sprintf("tok-%s-%d", strings.ReplaceAll(repo, "/", "-"), len(m.calls)), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (m *mintLog) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

func readRepoRelay(t *testing.T) (*Relay, *fakeGitHub, *mintLog, *[]Event) {
	t.Helper()
	r, gh, evs := relayFor(t, Policy{Repo: "acme/app", Access: Write, Push: []RefRule{{Ref: "refs/heads/mesh/g-1"}},
		ReadRepos: []string{"acme/lib"}})
	m := &mintLog{}
	r.Mint = m.mint
	return r, gh, m, evs
}

// A read repo's fetch is relayed upstream on a read token minted for
// that repository alone, cached for its next request, and its audit
// names it; the job's own repository keeps a token of its own.
func TestRelayFetchesAReadRepoOnItsOwnReadToken(t *testing.T) {
	r, gh, m, evs := readRepoRelay(t)
	if rec := do(r, "GET", "/git/ACME/Lib.git/info/refs?service=git-upload-pack", nil, nil); rec.Code != 200 {
		t.Fatalf("info/refs: %d %s", rec.Code, rec.Body)
	}
	if rec := do(r, "POST", "/git/acme/lib.git/git-upload-pack", []byte("0000"), nil); rec.Code != 200 {
		t.Fatalf("upload-pack: %d %s", rec.Code, rec.Body)
	}
	if got := m.seen(); len(got) != 1 || got[0] != "acme/lib read" {
		t.Fatalf("mints %v, want one read token for acme/lib", got)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok-acme-lib-1"))
	for i, req := range gh.reqs {
		if !strings.HasPrefix(req.URL.Path, "/acme/lib.git/") || gh.auth[i] != want {
			t.Fatalf("upstream request %d: %s with %q", i, req.URL.Path, gh.auth[i])
		}
	}
	for _, e := range *evs {
		if e.Outcome != "ok" || !strings.EqualFold(e.Repo, "acme/lib") || e.Service != "upload-pack" {
			t.Fatalf("audit %+v", e)
		}
	}

	if rec := do(r, "GET", "/git/acme/app.git/info/refs?service=git-upload-pack", nil, nil); rec.Code != 200 {
		t.Fatalf("own info/refs: %d", rec.Code)
	}
	if got := m.seen(); len(got) != 2 || got[1] != "acme/app write" {
		t.Fatalf("mints %v, want the own repository's at write", got)
	}
	if last := gh.auth[len(gh.auth)-1]; last != "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:tok-acme-app-2")) {
		t.Fatalf("the own repository went up with %q", last)
	}
}

// A push to a read repo, its advertisement or its pack, is refused and
// audited before anything is minted or forwarded; a repository neither
// the job's nor a read repo is refused as before.
func TestRelayRefusesAPushToAReadRepoWithoutMinting(t *testing.T) {
	r, gh, m, evs := readRepoRelay(t)
	if rec := do(r, "GET", "/git/acme/lib.git/info/refs?service=git-receive-pack", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("receive-pack advertisement: %d", rec.Code)
	}
	body := pushBody(zeroID + " 1111111111111111111111111111111111111111 refs/heads/mesh/g-1")
	if rec := do(r, "POST", "/git/Acme/Lib.git/git-receive-pack", body, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("receive-pack: %d", rec.Code)
	}
	if rec := do(r, "GET", "/git/acme/other.git/info/refs?service=git-upload-pack", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("another repository: %d", rec.Code)
	}
	if len(gh.reqs) != 0 || len(m.seen()) != 0 {
		t.Fatalf("%d upstream requests, mints %v", len(gh.reqs), m.seen())
	}
	if len(*evs) != 3 {
		t.Fatalf("audit %+v", *evs)
	}
	for i, e := range (*evs)[:2] {
		if e.Outcome != "refused" || !strings.Contains(e.Reason, "may only read") || !strings.EqualFold(e.Repo, "acme/lib") {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
	if e := (*evs)[2]; e.Outcome != "refused" || !strings.Contains(e.Reason, "is not this job's") {
		t.Fatalf("another repository's event: %+v", e)
	}
}

// A grant mints a read repo's token through its Mint, for that
// repository at read, and revokes it on Close with the own repository's.
func TestGrantRevokesItsReadReposTokens(t *testing.T) {
	gh := &fakeGitHub{}
	s, _, _ := startServer(t, gh)
	m := &mintLog{}
	rv := &revoker{}
	g, err := s.Open("lease-1", Policy{Repo: "acme/app", Access: Read, ReadRepos: []string{"acme/lib"}}, m.mint, rv.revoke)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"acme/lib", "acme/app", "acme/lib"} {
		if code := status(t, g.Base()+repo+".git/info/refs?service=git-upload-pack"); code != 200 {
			t.Fatalf("%s: %d", repo, code)
		}
	}
	if got := m.seen(); len(got) != 2 || got[0] != "acme/lib read" || got[1] != "acme/app read" {
		t.Fatalf("mints %v", got)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if len(rv.calls) != 1 || len(rv.calls[0]) != 2 {
		t.Fatalf("revoked %v, want both tokens once", rv.calls)
	}
	if _, err := s.Open("lease-2", Policy{Repo: "acme/app", ReadRepos: []string{"not-a-repo"}}, m.mint, nil); err == nil {
		t.Fatal("a read repo the vendor cannot name was opened")
	}
}

// Real git clones a read repo through the relay: from the node's mirror
// of it when the mirror holds the pack, upstream on the read repo's own
// read token when there is none; and a push from that clone is refused
// with the read repo unmoved.
func TestRelayClonesAReadRepoWithRealGit(t *testing.T) {
	run, root, bare, url := realBackend(t)
	must := func(dir string, args ...string) string {
		t.Helper()
		out, err := run(dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	mirror := filepath.Join(root, "lib-mirror.git")
	must(root, "clone", "-q", "--mirror", bare, mirror)
	var mu sync.Mutex
	var upstream, auth []string
	target, _ := neturl.Parse(url)
	backend := httputil.NewSingleHostReverseProxy(target)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstream = append(upstream, r.Method+" "+r.URL.Path)
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		backend.ServeHTTP(w, r)
	}))
	defer gh.Close()

	for _, withMirror := range []bool{true, false} {
		t.Run(fmt.Sprintf("mirror %t", withMirror), func(t *testing.T) {
			mu.Lock()
			upstream, auth = nil, nil
			mu.Unlock()
			m := &mintLog{}
			var events []Event
			// the backend serves acme/app; here it is the read repo of a
			// job whose own repository is acme/own
			relay := &Relay{
				Policy:   Policy{Repo: "acme/own", Access: Write, Push: []RefRule{{Ref: "refs/heads/mesh/g-1"}}, ReadRepos: []string{"acme/app"}},
				Upstream: gh.URL, Mint: m.mint,
				Audit: func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() },
			}
			if withMirror {
				relay.ReadMirrors = map[string]string{"acme/app": mirror}
			}
			front := httptest.NewServer(relay)
			defer front.Close()
			work := filepath.Join(root, fmt.Sprintf("work-%t", withMirror))
			must(root, "-c", "protocol.version=0", "clone", "-q", front.URL+"/git/acme/app.git", work)
			if b, _ := os.ReadFile(filepath.Join(work, "a.txt")); string(b) != "one\n" {
				t.Fatalf("the clone read %q", b)
			}
			if got := m.seen(); len(got) != 1 || got[0] != "acme/app read" {
				t.Fatalf("mints %v", got)
			}
			mu.Lock()
			posts := 0
			for i, u := range upstream {
				if strings.HasPrefix(u, "POST ") {
					posts++
				}
				if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok-acme-app-1")); auth[i] != want {
					t.Errorf("upstream %s with %q", u, auth[i])
				}
			}
			mu.Unlock()
			if withMirror && posts != 0 {
				t.Fatalf("the pack went upstream with a mirror: %v", upstream)
			}
			if !withMirror && posts == 0 {
				t.Fatalf("no pack went upstream without a mirror: %v", upstream)
			}

			before := must(bare, "rev-parse", "refs/heads/mesh/g-1")
			os.WriteFile(filepath.Join(work, "b.txt"), []byte("two\n"), 0o644)
			must(work, "add", ".")
			must(work, "commit", "-qm", "two")
			if out, err := run(work, "push", "origin", "HEAD:refs/heads/mesh/g-1"); err == nil {
				t.Fatalf("a push to the read repo went through:\n%s", out)
			}
			if after := must(bare, "rev-parse", "refs/heads/mesh/g-1"); after != before {
				t.Fatal("the read repo moved")
			}
			if got := m.seen(); len(got) != 1 {
				t.Fatalf("the push minted: %v", got)
			}
		})
	}
}
