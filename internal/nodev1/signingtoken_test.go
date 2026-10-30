package nodev1

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
	"projectmesh.io/mesh-cli/internal/sigstore"
)

// claimsOf is a JWT's claims, unverified.
func claimsOf(t *testing.T, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %d parts", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// A lease's node tools sign as its session with the ID token Fulcio
// takes — audience `sigstore`, minted unbound on the session's request
// credential (Latchkey ADR 017 decision 3) — never the mesh-session
// token, whose audience dl360's Fulcio refused ("expected audience
// sigstore got [mesh-session]", 2026-10-30). The token is kept until
// near its end, and a transcript is redacted of it.
func TestNodeToolsSignWithTheSessionsSigstoreToken(t *testing.T) {
	lk := newFakeLatchkey(t)
	key, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	cred, _ := lk.open("lease-1", key.Thumbprint())
	tokens := &sessionTokens{cred: cred, key: key, http: http.DefaultClient}
	ctx := context.Background()

	rt := &Runtime{Sigstore: &sigstore.Signer{FulcioURL: "http://fulcio.test", RekorURL: "http://rekor.test"}}
	b := &meshapi.Brief{JobID: "job-1", LeaseID: "lease-1", LeaseN: 1, NodeTools: []string{nodetools.RunSuiteTool}}
	_, rec := rt.nodeTools(b, nil, t.TempDir(), tokens, nil, nil, nil, nil)
	if rec == nil || rec.Signer == nil || rec.Signer.Token == nil {
		t.Fatal("the lease's recorder has no signing token")
	}
	tok, err := rec.Signer.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := claimsOf(t, tok)
	if c["aud"] != SigstoreAudience || !strings.HasPrefix(c["sub"].(string), "mesh:") {
		t.Fatalf("the signer presents aud %v sub %v, want aud %s and the session's subject", c["aud"], c["sub"], SigstoreAudience)
	}
	again, err := tokens.SigningToken(ctx)
	if err != nil || again != tok {
		t.Fatalf("the signing token was not kept: %v", err)
	}
	lk.mu.Lock()
	mints := lk.signingMints
	lk.mu.Unlock()
	if mints != 1 {
		t.Fatalf("Latchkey minted %d signing tokens, want 1", mints)
	}
	if !slices.Contains(tokens.secrets(), tok) {
		t.Fatal("the signing token is not among the lease's secrets")
	}
	// the mesh-session token is still what the relay holds, DPoP-bound
	st, err := tokens.Token(ctx)
	if err != nil || st == tok {
		t.Fatalf("the session token %v", err)
	}
}

// A signing token Latchkey refuses is said with its refusal, never the
// request credential.
func TestSigningTokenRefusals(t *testing.T) {
	lk := newFakeLatchkey(t)
	key, _ := dpop.NewKey()
	cred, _ := lk.open("lease-1", key.Thumbprint())
	bad := *cred
	bad.RequestToken = "lksr_" + strings.Repeat("9", 43)
	_, err := (&sessionTokens{cred: &bad, key: key, http: http.DefaultClient}).SigningToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "signing token (audience sigstore) was refused: 401") || strings.Contains(err.Error(), bad.RequestToken) {
		t.Fatalf("got %v", err)
	}
}
