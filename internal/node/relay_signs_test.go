package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
	"projectmesh.io/mesh-cli/internal/sockettest"
	"projectmesh.io/mesh-cli/internal/tools"
)

// A build session's mesh session is minted for audience mesh-session,
// DPoP-bound to the node's own in-memory key, as mesh node start builds
// SessionBearers (internal/commands/nodecmd.go; runsheet ADR 064 §5).
func TestRelaySignsEachRequest(t *testing.T) {
	t.Run("mint_asks_mesh_session", func(t *testing.T) {
		key, err := dpop.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		f := gatewaytest.New(machine)
		t.Cleanup(f.Close)
		f.WorkItems[item1] = map[string]any{"id": item1, "orgId": orgID, "status": "claimed", "nodeId": nodeA, "attempts": 1}
		gw := gateway.New(f.URL(), machine)
		b := &SessionBearers{GW: gw, Tokens: &TokenFiles{}, Key: key, Orgs: func() int { return 1 }}

		unbind, err := b.Open(context.Background(), gateway.WorkItem{ID: item1, OrgID: orgID, Attempts: 1}, nodeA)
		if err != nil {
			t.Fatal(err)
		}
		defer unbind()

		ms := f.MeshSessionFor(item1)
		if ms == nil || len(ms.Minted) == 0 {
			t.Fatalf("no token minted (%+v)", ms)
		}
		if len(ms.Audiences) != 1 || ms.Audiences[0] != MeshSessionAudience {
			t.Fatalf("audiences = %v, want [%s]", ms.Audiences, MeshSessionAudience)
		}
		tok := ms.Minted[0]
		if got := ms.JKT[tok]; got != key.Thumbprint() {
			t.Fatalf("token %q bound to %q, want %s", tok, got, key.Thumbprint())
		}
	})
	// Every /mcp request the session proxy forwards to the built-in
	// carries the session's token as DPoP and a proof of its own over the
	// node's key: the verifier accepts each, and no two share a jti
	// (runsheet ADR 064 §2, §5).
	t.Run("each_request_fresh_proof", func(t *testing.T) {
		key, err := dpop.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		f := gatewaytest.New(machine)
		t.Cleanup(f.Close)
		f.WorkItems[item1] = map[string]any{"id": item1, "orgId": orgID, "status": "claimed", "nodeId": nodeA, "attempts": 1}
		gw := gateway.New(f.URL(), machine)
		tf := &TokenFiles{}
		b := &SessionBearers{GW: gw, Tokens: tf, Key: key, Orgs: func() int { return 1 }}
		unbind, err := b.Open(context.Background(), gateway.WorkItem{ID: item1, OrgID: orgID, Attempts: 1}, nodeA)
		if err != nil {
			t.Fatal(err)
		}
		defer unbind()
		meta := harness.SessionMeta{ItemID: item1, Attempt: 1}
		bearer, err := tf.Source(harness.KindBuild, meta)
		if err != nil {
			t.Fatal(err)
		}
		signer := tf.KeyOf(meta)
		if signer == nil {
			t.Fatal("no DPoP key bound for the session")
		}

		var v dpoptest.Verifier
		var mu sync.Mutex
		type got struct {
			auth, proof string
			err         error
		}
		var seen []got
		mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			proof := r.Header.Get("DPoP")
			err := v.Verify(proof, dpoptest.Request{Method: r.Method, URL: "http://" + r.Host + r.URL.RequestURI(),
				Token: strings.TrimPrefix(auth, "DPoP "), JKT: key.Thumbprint()})
			mu.Lock()
			seen = append(seen, got{auth, proof, err})
			mu.Unlock()
			if err != nil {
				w.Header().Set("WWW-Authenticate", `DPoP error="invalid_dpop_proof"`)
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		}))
		defer mcp.Close()

		ups := upstreamsOf(map[string]tools.UpstreamSpec{tools.Builtin: {}}, mcp.URL+"/mcp", bearer, signer)
		p := sessionproxy.New(ups)
		sock := filepath.Join(sockettest.Dir(t, "rs"), ProxySocketName)
		if err := p.Listen(sock); err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
		for i := 0; i < 2; i++ {
			req, _ := http.NewRequest(http.MethodPost, "http://unix"+sessionproxy.Prefix+tools.Builtin,
				strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("request %d: proxy answered %d: %s", i, resp.StatusCode, body)
			}
		}

		ms := f.MeshSessionFor(item1)
		if ms == nil || len(ms.Minted) == 0 {
			t.Fatalf("no token minted (%+v)", ms)
		}
		tok := ms.Minted[len(ms.Minted)-1]
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != 2 {
			t.Fatalf("/mcp saw %d requests, want 2", len(seen))
		}
		jtis := map[string]bool{}
		for i, g := range seen {
			if g.auth != "DPoP "+tok {
				t.Errorf("request %d: Authorization %q, want DPoP %s", i, g.auth, tok)
			}
			if g.err != nil {
				t.Errorf("request %d: proof refused: %v", i, g.err)
			}
			jtis[proofJTI(t, g.proof)] = true
		}
		if len(jtis) != 2 {
			t.Fatalf("the two requests share a jti: %v", jtis)
		}
	})
}

// proofJTI is a DPoP proof's jti claim.
func proofJTI(t *testing.T, proof string) string {
	t.Helper()
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("proof %q is not a compact JWS", proof)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.JTI == "" {
		t.Fatalf("proof %q has no jti", proof)
	}
	return claims.JTI
}
