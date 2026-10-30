package gitrelay

import (
	"context"
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

// A relay with a mirror answers a fetch whose every want the mirror
// holds from it, and nothing of that fetch's pack is asked of upstream;
// a fetch of what the mirror lacks goes upstream; the ref
// advertisement is always upstream's, so a clone sees the vendor's refs
// now. Both git protocols, v2 and v0, are served from it.
func TestARelayFetchesFromItsMirrorAndFallsBackUpstream(t *testing.T) {
	for _, proto := range []string{"2", "0"} {
		t.Run("protocol v"+proto, func(t *testing.T) {
			run, root, bare, url := realBackend(t)
			must := func(dir string, args ...string) string {
				t.Helper()
				out, err := run(dir, args...)
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
				return strings.TrimSpace(out)
			}
			// the node's mirror: what upstream held when it last synced
			mirror := filepath.Join(root, "mirror.git")
			must(root, "clone", "-q", "--mirror", bare, mirror)
			var mu sync.Mutex
			var upstream []string
			target, _ := neturl.Parse(url)
			backend := httputil.NewSingleHostReverseProxy(target)
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				upstream = append(upstream, r.Method+" "+r.URL.Path)
				mu.Unlock()
				backend.ServeHTTP(w, r)
			}))
			defer gh.Close()
			var events []Event
			relay := &Relay{
				Policy:   Policy{Repo: "acme/app", Access: Read},
				Upstream: gh.URL, Mirror: mirror,
				Mint: func(context.Context, string, Access) (Token, error) {
					return Token{Value: "job-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
				},
				Audit: func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() },
			}
			front := httptest.NewServer(relay)
			defer front.Close()
			posts := func() int {
				mu.Lock()
				defer mu.Unlock()
				n := 0
				for _, u := range upstream {
					if strings.HasPrefix(u, "POST ") && strings.HasSuffix(u, "/git-upload-pack") {
						n++
					}
				}
				return n
			}

			work := filepath.Join(root, "work")
			must(root, "-c", "protocol.version="+proto, "clone", "-q", front.URL+"/git/acme/app.git", work)
			if b, _ := os.ReadFile(filepath.Join(work, "a.txt")); string(b) != "one\n" {
				t.Fatalf("the clone read %q", b)
			}
			fetchesUpstream := posts()
			if proto == "2" {
				// v2's ls-refs is a POST upstream answers; the fetch is not
				if fetchesUpstream != 1 {
					t.Fatalf("protocol v2: %d upload-pack posts went upstream, want ls-refs alone: %v", fetchesUpstream, upstream)
				}
			} else if fetchesUpstream != 0 {
				t.Fatalf("protocol v0: the pack went upstream: %v", upstream)
			}
			mirrored := false
			for _, e := range events {
				mirrored = mirrored || (e.Reason == "from the node's mirror" && e.Outcome == "ok" && e.Bytes > 0)
			}
			if !mirrored {
				t.Fatalf("no fetch was answered from the mirror: %+v", events)
			}

			// upstream moves on: the mirror lacks the new commit, so its
			// fetch goes upstream, and the checkout has it
			seed := filepath.Join(root, "seed")
			os.WriteFile(filepath.Join(seed, "b.txt"), []byte("two\n"), 0o644)
			must(seed, "add", ".")
			must(seed, "commit", "-qm", "two")
			must(seed, "push", "-q", bare, "main")
			before := posts()
			must(work, "-c", "protocol.version="+proto, "pull", "-q", "--ff-only")
			if b, _ := os.ReadFile(filepath.Join(work, "b.txt")); string(b) != "two\n" {
				t.Fatalf("the fetch past the mirror read %q", b)
			}
			if posts() <= before {
				t.Fatal("a fetch the mirror could not answer did not go upstream")
			}
		})
	}
}

// wantsOf reads both protocols' wants, and leaves a shallow fetch and
// v2's ls-refs upstream.
func TestWantsOf(t *testing.T) {
	pkt := func(lines ...string) []byte {
		var b strings.Builder
		for _, l := range lines {
			switch l {
			case "0000", "0001":
				b.WriteString(l)
			default:
				b.WriteString(hex4(len(l)+5) + l + "\n")
			}
		}
		return []byte(b.String())
	}
	a, c := strings.Repeat("a", 40), strings.Repeat("c", 40)
	for _, tc := range []struct {
		name  string
		body  []byte
		wants int
		fetch bool
	}{
		{"v2 fetch", pkt("command=fetch", "agent=git/2", "0001", "thin-pack", "want "+a, "want "+c, "done", "0000"), 2, true},
		{"v2 ls-refs", pkt("command=ls-refs", "0001", "peel", "0000"), 0, false},
		{"v0", pkt("want "+a+" multi_ack side-band-64k", "want "+c, "0000", "have "+c, "done"), 2, true},
		{"shallow", pkt("command=fetch", "0001", "want "+a, "deepen 1", "0000"), 0, false},
		{"garbage", []byte("zzzz"), 0, false},
	} {
		w, f := wantsOf(tc.body)
		if len(w) != tc.wants || f != tc.fetch {
			t.Errorf("%s: wants %v fetch %v", tc.name, w, f)
		}
	}
}

func hex4(n int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[n>>12&15], digits[n>>8&15], digits[n>>4&15], digits[n&15]})
}
