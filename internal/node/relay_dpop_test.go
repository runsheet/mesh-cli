package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
	"projectmesh.io/mesh-cli/internal/tools"
)

// The relay end to end (runsheet ADR 064 §2 and §5): a build's mesh
// session token is minted for audience mesh-session on a DPoP proof of
// the node's in-memory key; the session's proxy is given that key for
// the runsheet upstream, and every /mcp call through its socket reaches
// a verifying /mcp as `Authorization: DPoP <token>` with a fresh proof
// (its own jti, ath the token's hash, jkt the key's thumbprint); a
// refusal of the proof reaches the agent as -32603 naming it.
func TestRelaySignsEachRequest(t *testing.T) {
	const nodeID = "node-relay-1"
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	f := seed(t)
	f.SetItem(item1, map[string]any{"status": "claimed", "nodeId": nodeID})
	tf := &TokenFiles{Token: func(context.Context) (string, error) { return machine, nil }}
	b := &SessionBearers{GW: gateway.New(f.URL(), machine), Tokens: tf, Key: key}
	unbind, err := b.Open(context.Background(), gateway.WorkItem{ID: item1, Attempts: 0}, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	defer unbind()

	// the mint: audience mesh-session, the proof's key the node's
	ms := f.MeshSessionFor(item1)
	if ms == nil || len(ms.Minted) != 1 {
		t.Fatalf("want one token minted, got %+v", ms)
	}
	tok := ms.Minted[0]
	if ms.Audiences[0] != MeshSessionAudience || ms.JKT[tok] != key.Thumbprint() {
		t.Fatalf("minted %q for %q bound to %q, want %s bound to %s", tok, ms.Audiences[0], ms.JKT[tok], MeshSessionAudience, key.Thumbprint())
	}

	// the runsheet upstream signs with that key
	meta := harness.SessionMeta{NodeID: nodeID, ItemID: item1, Attempt: 0}
	px := &SessionProxies{Bearer: tf.Source, Key: tf.KeyOf, Root: shortProxyRoot(t)}
	if got := px.keyOf(meta); got != sessionproxy.Signer(key) {
		t.Fatalf("keyOf = %v, want the node's key", got)
	}
	specs := map[string]tools.UpstreamSpec{tools.Builtin: {}}
	if up, ok := upstreamsOf(specs, "http://unused/mcp", tf.Token, px.keyOf(meta))[tools.Builtin].(sessionproxy.Runsheet); !ok || up.Key != sessionproxy.Signer(key) {
		t.Fatalf("runsheet upstream %+v, want one signed by the node's key", up)
	}

	// a fake /mcp verifying every proof by ADR 064 §2
	var v dpoptest.Verifier
	var refuse atomic.Bool
	var mu sync.Mutex
	var proofs, auths []string
	var verr []error
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		proofs = append(proofs, r.Header.Get("DPoP"))
		auths = append(auths, r.Header.Get("Authorization"))
		if refuse.Load() {
			w.Header().Set("WWW-Authenticate", `DPoP error="invalid_dpop_proof", error_description="proof iat is stale"`)
			http.Error(w, "invalid_dpop_proof", http.StatusUnauthorized)
			return
		}
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "DPoP ")
		if !ok {
			http.Error(w, "DPoP scheme required", http.StatusUnauthorized)
			return
		}
		if err := v.Verify(r.Header.Get("DPoP"), dpoptest.Request{Method: r.Method, URL: "http://" + r.Host + r.URL.RequestURI(), Token: bearer, JKT: key.Thumbprint()}); err != nil {
			verr = append(verr, err)
			w.Header().Set("WWW-Authenticate", `DPoP error="invalid_dpop_proof"`)
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		var m map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&m)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "up-1")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"runsheet ok"}]}}`, m["id"])
	}))
	defer mcp.Close()

	sp, err := px.open(&harness.Job{Kind: harness.KindBuild, Meta: meta}, "", mcp.URL+"/mcp", specs)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	// two /mcp calls through the session's socket, each freshly signed
	got, err := callThrough(sp.Socket, tools.Builtin)
	if err != nil {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("call through the proxy: %v (verifier: %v)", err, verr)
	}
	if !strings.Contains(got, "runsheet ok") {
		t.Fatalf("call answered %s", got)
	}
	mu.Lock()
	if len(proofs) != 2 || len(verr) != 0 {
		t.Fatalf("/mcp saw %d requests, refused %v; want 2, each accepted", len(proofs), verr)
	}
	jtis := map[string]bool{}
	for i, p := range proofs {
		if auths[i] != "DPoP "+tok {
			t.Fatalf("request %d: Authorization %q, want DPoP %s", i, auths[i], tok)
		}
		jti, err := dpoptest.ProofJTI(p)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if jtis[jti] {
			t.Fatalf("request %d reuses jti %s", i, jti)
		}
		jtis[jti] = true
	}
	proofs = nil
	mu.Unlock()

	// a refusal of the proof: -32603 naming it, never retried
	refuse.Store(true)
	status, body := postThrough(t, sp.Socket, tools.Builtin, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"look","arguments":{}}}`)
	var rpc struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &rpc); err != nil {
		t.Fatalf("refusal answered %d, not a JSON-RPC error: %s", status, body)
	}
	if rpc.Error.Code != -32603 {
		t.Fatalf("refusal's code %d, want -32603: %s", rpc.Error.Code, body)
	}
	for _, want := range []string{"upstream refused the DPoP proof", "invalid_dpop_proof", "proof iat is stale"} {
		if !strings.Contains(rpc.Error.Message, want) {
			t.Errorf("refusal %q does not name %q", rpc.Error.Message, want)
		}
	}
	if strings.Contains(body, tok) {
		t.Errorf("the refusal carries the token: %s", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(proofs) != 1 {
		t.Fatalf("/mcp saw %d requests for the refused call, want 1 (no retry)", len(proofs))
	}
}

// postThrough posts one JSON-RPC message to server on socket and
// answers the status and body.
func postThrough(t *testing.T, socket, server, msg string) (int, string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	req, _ := http.NewRequest(http.MethodPost, "http://unix/mcp/"+server, strings.NewReader(msg))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
