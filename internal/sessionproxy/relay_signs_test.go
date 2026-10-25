package sessionproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
)

// A DPoP Runsheet upstream signs each request with a fresh proof
// (runsheet ADR 064 §2): a 401 invalid_token is forwarded again exactly
// once, re-authorized with a fresh proof, and a 401 refusing the proof
// itself (invalid_dpop_proof) is never retried.
func TestRelaySignsEachRequest(t *testing.T) {
	// With a Key the built-in goes as `Authorization: DPoP <token>` with a
	// fresh proof of the request's method and URL and the token's hash by
	// that key, every request — what runsheet's /mcp takes of a session
	// token (runsheet ADR 064 §2) — each proof its own jti, and a DPoP the
	// agent sends is never forwarded.
	t.Run("fresh_proof_per_request", func(t *testing.T) {
		key, err := dpop.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		var v dpoptest.Verifier
		var s seen
		var mu sync.Mutex
		var verr []error
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.add(r)
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "DPoP ")
			if !ok {
				http.Error(w, "DPoP scheme required", http.StatusUnauthorized)
				return
			}
			if err := v.Verify(r.Header.Get("DPoP"), dpoptest.Request{Method: r.Method, URL: "http://" + r.Host + r.URL.RequestURI(), Token: tok, JKT: key.Thumbprint()}); err != nil {
				mu.Lock()
				verr = append(verr, err)
				mu.Unlock()
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
		for i := 0; i < 2; i++ {
			resp, body := post(t, c, "runsheet", map[string]string{"DPoP": "agent-proof", "Authorization": "Bearer agent"}, initialize)
			if resp.StatusCode != 200 {
				mu.Lock()
				t.Fatalf("request %d: status %d: %s (%v)", i, resp.StatusCode, body, verr)
			}
		}
		reqs := s.all()
		if len(reqs) != 2 {
			t.Fatalf("upstream saw %d requests, want 2", len(reqs))
		}
		for i, r := range reqs {
			if got := r.Header.Get("Authorization"); got != "DPoP "+bearerNew {
				t.Fatalf("request %d: Authorization %q, want DPoP %s", i, got, bearerNew)
			}
			if r.Header.Get("DPoP") == "" || r.Header.Get("DPoP") == "agent-proof" {
				t.Fatalf("request %d: DPoP %q, want the proxy's own proof", i, r.Header.Get("DPoP"))
			}
			if jkt, err := dpoptest.ProofJKT(r.Header.Get("DPoP")); err != nil || jkt != key.Thumbprint() {
				t.Fatalf("request %d: proof jkt %q (%v), want %s", i, jkt, err, key.Thumbprint())
			}
		}
		if a, b := proofJTI(t, reqs[0].Header.Get("DPoP")), proofJTI(t, reqs[1].Header.Get("DPoP")); a == b {
			t.Fatalf("both requests carry jti %s", a)
		}
	})

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

	// A 401 refusing the proof itself is not a stale token: the agent gets
	// -32603 naming invalid_dpop_proof and its description, and the
	// upstream is asked once, never again with another proof.
	t.Run("proof_refusal", func(t *testing.T) {
		key, err := dpop.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		var s seen
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.add(r)
			w.Header().Set("WWW-Authenticate", `DPoP error="invalid_dpop_proof", error_description="proof htu does not match"`)
			http.Error(w, "invalid_dpop_proof", http.StatusUnauthorized)
		}))
		defer up.Close()

		p := New(map[string]Upstream{"runsheet": Runsheet{URL: up.URL + "/mcp", Key: key,
			Bearer: func(context.Context) (string, error) { return bearerNew, nil }}})
		c, _ := listen(t, p)
		resp, body := post(t, c, "runsheet", nil, initialize)
		var rpc struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &rpc); err != nil {
			t.Fatalf("client got %d, not a JSON-RPC error: %s", resp.StatusCode, body)
		}
		if rpc.Error.Code != -32603 {
			t.Fatalf("JSON-RPC error code %d, want -32603: %s", rpc.Error.Code, body)
		}
		for _, want := range []string{"upstream refused the DPoP proof", "invalid_dpop_proof", "proof htu does not match"} {
			if !strings.Contains(rpc.Error.Message, want) {
				t.Errorf("message %q does not name %q", rpc.Error.Message, want)
			}
		}
		if strings.Contains(body, bearerNew) {
			t.Errorf("the error carries the bearer: %s", body)
		}
		if n := len(s.all()); n != 1 {
			t.Fatalf("upstream saw %d requests, want exactly 1 (no retry)", n)
		}
	})
}

// proofJTI is a DPoP proof's jti (dpoptest.ProofJTI), failing t without one.
func proofJTI(t *testing.T, proof string) string {
	t.Helper()
	jti, err := dpoptest.ProofJTI(proof)
	if err != nil {
		t.Fatal(err)
	}
	return jti
}
