package sessionproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
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

		// The retry is signed anew: another DPoP header, another jti, and a
		// proof the verifier accepts for the retried request's method, URL
		// and token.
		t.Run("fresh_proof", func(t *testing.T) {
			key, err := dpop.NewKey()
			if err != nil {
				t.Fatal(err)
			}
			var s seen
			var n atomic.Int32
			var v dpoptest.Verifier
			verified := make(chan error, 1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s.add(r)
				if n.Add(1) == 1 {
					w.Header().Set("WWW-Authenticate", `DPoP error="invalid_token"`)
					http.Error(w, "invalid_token", http.StatusUnauthorized)
					return
				}
				err := v.Verify(r.Header.Get("DPoP"), dpoptest.Request{Method: r.Method,
					URL: "http://" + r.Host + r.URL.RequestURI(), Token: bearerNew, JKT: key.Thumbprint()})
				verified <- err
				if err != nil {
					http.Error(w, err.Error(), http.StatusUnauthorized)
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
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("client got %d, want 200: %s", resp.StatusCode, body)
			}
			reqs := s.all()
			if len(reqs) != 2 {
				t.Fatalf("upstream saw %d requests, want 2", len(reqs))
			}
			select {
			case err := <-verified:
				if err != nil {
					t.Fatalf("the retry's proof does not verify: %v", err)
				}
			default:
				t.Fatal("the retry reached no verifier")
			}
			first, second := reqs[0].Header.Get("DPoP"), reqs[1].Header.Get("DPoP")
			if first == "" || second == "" {
				t.Fatalf("a request carries no DPoP proof: %q, %q", first, second)
			}
			if first == second {
				t.Fatal("the retry reused the first proof")
			}
			if a, b := proofJTI(t, first), proofJTI(t, second); a == b {
				t.Fatalf("the retry's jti %s is the first proof's", b)
			}
		})
	})
}

// proofJTI reads a DPoP proof's jti from its JWT payload.
func proofJTI(t *testing.T, proof string) string {
	t.Helper()
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("proof %q is not a compact JWS", proof)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("proof payload: %v", err)
	}
	var claims struct {
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("proof payload: %v", err)
	}
	if claims.JTI == "" {
		t.Fatal("proof carries no jti")
	}
	return claims.JTI
}
