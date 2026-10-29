package sessionproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
)

// With a Key the built-in goes as `Authorization: DPoP <token>` with a
// fresh proof of the request's method and URL and the token's hash by
// that key, every request — what runsheet's /mcp takes of a session
// token (runsheet ADR 064 §2) — and a DPoP the agent sends is never
// forwarded.
func TestProxyProductSignsEachRequestWithTheSessionsKey(t *testing.T) {
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	var v dpoptest.Verifier
	var s seen
	var verr []error
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.add(r)
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "DPoP ")
		if !ok {
			http.Error(w, "DPoP scheme required", http.StatusUnauthorized)
			return
		}
		if err := v.Verify(r.Header.Get("DPoP"), dpoptest.Request{Method: r.Method, URL: "http://" + r.Host + r.URL.RequestURI(), Token: tok, JKT: key.Thumbprint()}); err != nil {
			verr = append(verr, err)
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer up.Close()

	p := New(map[string]Upstream{"runsheet": Product{URL: up.URL + "/mcp", Key: key,
		Bearer: func(context.Context) (string, error) { return bearerNew, nil }}})
	c, _ := listen(t, p)
	for i := 0; i < 2; i++ {
		resp, body := post(t, c, "runsheet", map[string]string{"DPoP": "agent-proof", "Authorization": "Bearer agent"}, initialize)
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d: %s (%v)", i, resp.StatusCode, body, verr)
		}
	}
	reqs := s.all()
	if len(reqs) != 2 || reqs[0].Header.Get("DPoP") == reqs[1].Header.Get("DPoP") {
		t.Fatalf("want two requests, each with its own proof: %d", len(reqs))
	}
	for _, r := range reqs {
		if r.Header.Get("DPoP") == "agent-proof" || r.Header.Get("Authorization") != "DPoP "+bearerNew {
			t.Fatalf("upstream saw Authorization %q, DPoP %q", r.Header.Get("Authorization"), r.Header.Get("DPoP"))
		}
	}
}
