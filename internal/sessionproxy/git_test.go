package sessionproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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
