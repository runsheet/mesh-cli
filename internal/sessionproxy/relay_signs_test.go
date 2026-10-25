package sessionproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"projectmesh.io/mesh-cli/internal/dpop"
)

// A 401 invalid_token from runsheet is forwarded again exactly once,
// re-authorized with a fresh proof (runsheet ADR 064 §2): the retry's
// answer reaches the client, and a second 401 is the answer, no third
// request.
func TestRelaySignsEachRequest(t *testing.T) {
	t.Run("invalid_token_retries", func(t *testing.T) {
		t.Run("once", func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				failures int32
				status   int
			}{
				{"then 200", 1, http.StatusOK},
				{"twice", 2, http.StatusUnauthorized},
			} {
				t.Run(tc.name, func(t *testing.T) {
					key, err := dpop.NewKey()
					if err != nil {
						t.Fatal(err)
					}
					var s seen
					var n atomic.Int32
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						s.add(r)
						if n.Add(1) <= tc.failures {
							w.Header().Set("WWW-Authenticate", `DPoP error="invalid_token"`)
							http.Error(w, "invalid_token", http.StatusUnauthorized)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
					}))
					defer up.Close()

					p := New(map[string]Upstream{"runsheet": Runsheet{URL: up.URL + "/mcp", Key: key,
						Bearer: func(context.Context) (string, error) { return bearerNew, nil }}})
					c, _ := listen(t, p)
					resp, body := post(t, c, "runsheet", nil, initialize)
					if resp.StatusCode != tc.status {
						t.Fatalf("client got %d, want %d: %s", resp.StatusCode, tc.status, body)
					}
					reqs := s.all()
					if len(reqs) != 2 {
						t.Fatalf("upstream saw %d requests, want exactly 2", len(reqs))
					}
					for i, r := range reqs {
						if got := r.Header.Get("Authorization"); got != "DPoP "+bearerNew {
							t.Errorf("request %d: Authorization %q, want DPoP %s", i, got, bearerNew)
						}
						if r.Header.Get("DPoP") == "" {
							t.Errorf("request %d carries no DPoP proof", i)
						}
					}
					if reqs[0].Header.Get("DPoP") == reqs[1].Header.Get("DPoP") {
						t.Errorf("the retry reused the first proof")
					}
				})
			}
		})
	})
}
