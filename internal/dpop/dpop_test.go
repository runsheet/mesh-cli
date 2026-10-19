package dpop_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
)

const mcpURL = "https://api.runsheet.dev/mcp"

func newKey(t *testing.T) *dpop.Key {
	t.Helper()
	k, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func proof(t *testing.T, k *dpop.Key, method, url, token string) string {
	t.Helper()
	p, err := k.Proof(method, url, token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// parts decodes a proof's header and claims into maps.
func parts(t *testing.T, p string) (head, claims map[string]any) {
	t.Helper()
	seg := strings.Split(p, ".")
	if len(seg) != 3 {
		t.Fatalf("proof has %d parts, want 3", len(seg))
	}
	for i, dst := range []*map[string]any{&head, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(seg[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatal(err)
		}
	}
	return head, claims
}

// forge assembles a proof from a header, claims and a signature of a
// test's choosing.
func forge(head, claims map[string]any, sig string) string {
	h, _ := json.Marshal(head)
	c, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c) + "." + sig
}

func TestProof(t *testing.T) {
	k := newKey(t)

	t.Run("two proofs for the same request carry different jti", func(t *testing.T) {
		_, a := parts(t, proof(t, k, "POST", mcpURL, "tok"))
		_, b := parts(t, proof(t, k, "POST", mcpURL, "tok"))
		if a["jti"] == "" || a["jti"] == b["jti"] {
			t.Fatalf("jti %v and %v, want two different", a["jti"], b["jti"])
		}
	})

	t.Run("htu drops query and fragment", func(t *testing.T) {
		_, c := parts(t, proof(t, k, "POST", mcpURL+"?session=1&x=y#frag", "tok"))
		if c["htu"] != mcpURL {
			t.Fatalf("htu %v, want %s", c["htu"], mcpURL)
		}
		if c["htm"] != "POST" {
			t.Fatalf("htm %v, want POST", c["htm"])
		}
	})

	t.Run("ath is the token's base64url SHA-256", func(t *testing.T) {
		_, c := parts(t, proof(t, k, "POST", mcpURL, "the-token"))
		sum := sha256.Sum256([]byte("the-token"))
		if want := base64.RawURLEncoding.EncodeToString(sum[:]); c["ath"] != want {
			t.Fatalf("ath %v, want %s", c["ath"], want)
		}
	})

	t.Run("ath is absent for an empty token", func(t *testing.T) {
		_, c := parts(t, proof(t, k, "POST", "https://auth.runsheet.dev/token", ""))
		if _, ok := c["ath"]; ok {
			t.Fatalf("ath %v on a proof with no token", c["ath"])
		}
	})

	t.Run("header jwk has no d", func(t *testing.T) {
		h, _ := parts(t, proof(t, k, "POST", mcpURL, "tok"))
		if h["typ"] != "dpop+jwt" || h["alg"] != "ES256" {
			t.Fatalf("header %v", h)
		}
		jwk, _ := h["jwk"].(map[string]any)
		if _, ok := jwk["d"]; ok {
			t.Fatalf("jwk carries d: %v", jwk)
		}
		if jwk["kty"] != "EC" || jwk["crv"] != "P-256" || len(jwk) != 4 {
			t.Fatalf("jwk %v, want kty, crv, x, y alone", jwk)
		}
	})

	t.Run("signature is 64-byte r||s", func(t *testing.T) {
		seg := strings.Split(proof(t, k, "POST", mcpURL, "tok"), ".")
		sig, err := base64.RawURLEncoding.DecodeString(seg[2])
		if err != nil || len(sig) != 64 {
			t.Fatalf("signature %d bytes (%v), want 64", len(sig), err)
		}
	})

	t.Run("iat is the key's clock", func(t *testing.T) {
		at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
		k2 := newKey(t)
		k2.Now = func() time.Time { return at }
		_, c := parts(t, proof(t, k2, "POST", mcpURL, "tok"))
		if c["iat"] != float64(at.Unix()) {
			t.Fatalf("iat %v, want %d", c["iat"], at.Unix())
		}
	})

	t.Run("refuses a relative URL and no method", func(t *testing.T) {
		if _, err := k.Proof("POST", "/mcp", "tok"); err == nil {
			t.Fatal("proof for a relative URL")
		}
		if _, err := k.Proof("", mcpURL, "tok"); err == nil {
			t.Fatal("proof with no method")
		}
	})
}

func TestThumbprintIsRFC7638(t *testing.T) {
	// RFC 7638 §3.1 gives an RSA example; this is the EC form of the same
	// rule, worked by hand: the required members in lexical order.
	j := dpop.JWK{Kty: "EC", Crv: "P-256", X: "xx", Y: "yy"}
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"xx","y":"yy"}`))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); dpop.Thumbprint(j) != want {
		t.Fatalf("thumbprint %s, want %s", dpop.Thumbprint(j), want)
	}
	k := newKey(t)
	if k.Thumbprint() != dpop.Thumbprint(k.PublicJWK()) {
		t.Fatal("key's thumbprint is not its jwk's")
	}
}

func TestKeyPrintsNoPrivateMaterial(t *testing.T) {
	k := newKey(t)
	want := "dpop.Key(jkt=" + k.Thumbprint() + ")"
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%d"} {
		for _, v := range []any{k, *k} {
			if got := fmt.Sprintf(verb, v); got != want {
				t.Errorf("Sprintf(%q, %T) = %q, want %q", verb, v, got, want)
			}
		}
	}
	b, err := json.Marshal(k)
	if err != nil || string(b) != `{"jkt":"`+k.Thumbprint()+`"}` {
		t.Fatalf("json %s (%v)", b, err)
	}
}

func TestVerifier(t *testing.T) {
	k := newKey(t)
	req := dpoptest.Request{Method: "POST", URL: mcpURL + "?q=1", Token: "tok", JKT: k.Thumbprint()}

	t.Run("accepts the proof under the key's thumbprint", func(t *testing.T) {
		var v dpoptest.Verifier
		if err := v.Verify(proof(t, k, "POST", mcpURL, "tok"), req); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("accepts the mint proof with no token", func(t *testing.T) {
		var v dpoptest.Verifier
		mint := dpoptest.Request{Method: "POST", URL: "https://auth.runsheet.dev/token", JKT: k.Thumbprint()}
		if err := v.Verify(proof(t, k, "POST", mint.URL, ""), mint); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refuses it under another key's thumbprint", func(t *testing.T) {
		var v dpoptest.Verifier
		other := req
		other.JKT = newKey(t).Thumbprint()
		if err := v.Verify(proof(t, k, "POST", mcpURL, "tok"), other); err == nil || !strings.Contains(err.Error(), "thumbprint") {
			t.Fatalf("err %v, want a thumbprint refusal", err)
		}
	})

	t.Run("refuses another key's proof under this key's thumbprint", func(t *testing.T) {
		var v dpoptest.Verifier
		if err := v.Verify(proof(t, newKey(t), "POST", mcpURL, "tok"), req); err == nil {
			t.Fatal("accepted")
		}
	})

	t.Run("refuses a replayed jti", func(t *testing.T) {
		var v dpoptest.Verifier
		p := proof(t, k, "POST", mcpURL, "tok")
		if err := v.Verify(p, req); err != nil {
			t.Fatal(err)
		}
		if err := v.Verify(p, req); err == nil || !strings.Contains(err.Error(), "jti") {
			t.Fatalf("err %v, want a jti refusal", err)
		}
	})

	refusals := []struct {
		name string
		p    func() string
		req  func(dpoptest.Request) dpoptest.Request
		now  func() time.Time
		want string
	}{
		{name: "htm", req: func(r dpoptest.Request) dpoptest.Request { r.Method = "GET"; return r }, want: "htm"},
		{name: "htu", req: func(r dpoptest.Request) dpoptest.Request { r.URL = "https://api.runsheet.dev/graphql"; return r }, want: "htu"},
		{name: "ath", req: func(r dpoptest.Request) dpoptest.Request { r.Token = "other"; return r }, want: "ath"},
		{name: "ath on no token", req: func(r dpoptest.Request) dpoptest.Request { r.Token = ""; return r }, want: "ath"},
		{name: "no ath for a token", p: func() string { return proof(t, k, "POST", mcpURL, "") }, want: "ath"},
		{name: "iat too old", now: func() time.Time { return time.Now().Add(2 * time.Minute) }, want: "iat"},
		{name: "iat too new", now: func() time.Time { return time.Now().Add(-2 * time.Minute) }, want: "iat"},
		{name: "typ", p: tamper(t, k, func(h, _ map[string]any) { h["typ"] = "JWT" }), want: "typ"},
		{name: "alg", p: tamper(t, k, func(h, _ map[string]any) { h["alg"] = "RS256" }), want: "alg"},
		{name: "alg none", p: tamper(t, k, func(h, _ map[string]any) { h["alg"] = "none" }), want: "alg"},
		{name: "jwk with d", p: tamper(t, k, func(h, _ map[string]any) { h["jwk"].(map[string]any)["d"] = "AAAA" }), want: "private"},
		{name: "jwk off the curve", p: tamper(t, k, func(h, _ map[string]any) { h["jwk"].(map[string]any)["y"] = h["jwk"].(map[string]any)["x"] }), want: "P-256"},
		{name: "claims changed after signing", p: tamper(t, k, func(_, c map[string]any) { c["htm"] = "POST " }), want: "signature"},
	}
	for _, tc := range refusals {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			p := proof(t, k, "POST", mcpURL, "tok")
			if tc.p != nil {
				p = tc.p()
			}
			r := req
			if tc.req != nil {
				r = tc.req(r)
			}
			v := dpoptest.Verifier{Now: tc.now}
			err := v.Verify(p, r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// tamper answers a proof whose header or claims were changed after it was
// signed, keeping the original signature.
func tamper(t *testing.T, k *dpop.Key, change func(head, claims map[string]any)) func() string {
	return func() string {
		p := proof(t, k, "POST", mcpURL, "tok")
		h, c := parts(t, p)
		change(h, c)
		return forge(h, c, p[strings.LastIndex(p, ".")+1:])
	}
}
