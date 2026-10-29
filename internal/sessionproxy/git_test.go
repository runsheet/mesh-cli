package sessionproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gitrelay"
)

func TestProxyServesGitBesideMCP(t *testing.T) {
	p := New(nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/git/acme/app.git/info/refs?service=git-upload-pack", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no relay: got %d", rec.Code)
	}
	p.Git = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "git:"+r.URL.Path) })
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/git/acme/app.git/info/refs", nil))
	if rec.Body.String() != "git:/git/acme/app.git/info/refs" {
		t.Fatalf("git path not handed to the relay: %q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("an MCP path reached the relay: %d", rec.Code)
	}
}

// A session in a pod pushes through the proxy's /git/ route, which is
// the lease's grant: the same push rules hold there as on the loopback
// capability (Mesh v1 PR 1.10). A delete and a ref no rule names are
// refused before the vendor is reached, and both are audited.
func TestProxyGitRouteHoldsThePushRules(t *testing.T) {
	vendor := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { vendor++ }))
	t.Cleanup(up.Close)
	var refused []string
	s := &gitrelay.Server{Upstream: up.URL, Audit: func(e gitrelay.Event) {
		if e.Outcome == "refused" {
			refused = append(refused, e.Reason)
		}
	}}
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g, err := s.Open("lease-1", gitrelay.Policy{Repo: "acme/app", Access: gitrelay.Write, Push: []gitrelay.RefRule{{Ref: "refs/heads/mesh/*", FastForwardOnly: true}}},
		func(context.Context) (gitrelay.Token, error) {
			return gitrelay.Token{Value: "t", ExpiresAt: time.Now().Add(time.Hour)}, nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := New(nil)
	p.Git = g
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	for name, cmd := range map[string]string{
		"a delete":        strings.Repeat("1", 40) + " " + strings.Repeat("0", 40) + " refs/heads/mesh/a",
		"an unlisted ref": strings.Repeat("0", 40) + " " + strings.Repeat("1", 40) + " refs/heads/main",
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("POST", "/git/acme/app.git/git-receive-pack", strings.NewReader(pkt(cmd+"\x00report-status\n")+"0000PACK")))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: got %d", name, rec.Code)
		}
	}
	if vendor != 0 || len(refused) != 2 {
		t.Fatalf("the vendor saw %d requests; refusals %v", vendor, refused)
	}
}
