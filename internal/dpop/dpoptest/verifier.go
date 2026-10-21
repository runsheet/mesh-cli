// Package dpoptest is a fake DPoP verifier: it checks a proof by every rule
// of runsheet ADR 064 §2, as the gateway's /mcp and Latchkey's mint do, so
// the relay's tests can be run against it before either side is live.
package dpoptest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
)

// DefaultSkew is how far a proof's iat may be from the verifier's clock.
const DefaultSkew = 60 * time.Second

// Request is what a proof is checked against: the request it rode on, the
// access token it carried ("" for the mint, whose proof has no ath) and the
// thumbprint the token is bound to.
type Request struct {
	Method string
	URL    string
	Token  string
	JKT    string
}

// Verifier checks proofs and remembers every jti it has accepted. The zero
// value is ready to use.
type Verifier struct {
	// Now is the verifier's clock; nil is time.Now.
	Now func() time.Time
	// Skew bounds |iat - now|; zero is DefaultSkew.
	Skew time.Duration

	mu   sync.Mutex
	seen map[string]bool
}

// Verify answers nil when proof satisfies every ADR 064 §2 rule for req,
// else the first rule it breaks. An accepted proof's jti is remembered, so
// the same proof is refused a second time.
func (v *Verifier) Verify(proof string, req Request) error {
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		return errors.New("dpop: proof is not a compact JWS")
	}
	rawHead, err := decode(parts[0])
	if err != nil {
		return fmt.Errorf("dpop: header: %w", err)
	}
	rawClaims, err := decode(parts[1])
	if err != nil {
		return fmt.Errorf("dpop: claims: %w", err)
	}
	sig, err := decode(parts[2])
	if err != nil {
		return fmt.Errorf("dpop: signature: %w", err)
	}

	var head struct {
		Typ string         `json:"typ"`
		Alg string         `json:"alg"`
		JWK map[string]any `json:"jwk"`
	}
	if err := json.Unmarshal(rawHead, &head); err != nil {
		return fmt.Errorf("dpop: header: %w", err)
	}
	if head.Typ != dpop.Typ {
		return fmt.Errorf("dpop: typ %q is not %s", head.Typ, dpop.Typ)
	}
	if head.Alg != dpop.Alg {
		return fmt.Errorf("dpop: alg %q is not %s", head.Alg, dpop.Alg)
	}
	jwk, pub, err := publicJWK(head.JWK)
	if err != nil {
		return err
	}
	if len(sig) != 64 {
		return fmt.Errorf("dpop: signature is %d bytes, not ES256's 64", len(sig))
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, sum[:], r, s) {
		return errors.New("dpop: signature does not verify under the header's jwk")
	}
	if got := dpop.Thumbprint(jwk); got != req.JKT {
		return fmt.Errorf("dpop: jwk thumbprint %s is not the token's jkt %s", got, req.JKT)
	}

	var claims struct {
		JTI string  `json:"jti"`
		HTM string  `json:"htm"`
		HTU string  `json:"htu"`
		IAT *int64  `json:"iat"`
		Ath *string `json:"ath"`
	}
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		return fmt.Errorf("dpop: claims: %w", err)
	}
	if claims.HTM != req.Method {
		return fmt.Errorf("dpop: htm %q is not the request's %q", claims.HTM, req.Method)
	}
	want, err := dpop.HTU(req.URL)
	if err != nil {
		return err
	}
	if claims.HTU != want {
		return fmt.Errorf("dpop: htu %q is not the request's %q", claims.HTU, want)
	}
	if claims.IAT == nil {
		return errors.New("dpop: no iat")
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	skew := v.Skew
	if skew == 0 {
		skew = DefaultSkew
	}
	if d := now().Sub(time.Unix(*claims.IAT, 0)); d > skew || d < -skew {
		return fmt.Errorf("dpop: iat is %s from now, past %s", d, skew)
	}
	switch {
	case req.Token == "" && claims.Ath != nil:
		return errors.New("dpop: ath on a request with no token")
	case req.Token != "" && claims.Ath == nil:
		return errors.New("dpop: no ath on a request with a token")
	case req.Token != "" && *claims.Ath != dpop.Ath(req.Token):
		return errors.New("dpop: ath is not the token's hash")
	}
	if claims.JTI == "" {
		return errors.New("dpop: no jti")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen[claims.JTI] {
		return fmt.Errorf("dpop: jti %s already seen", claims.JTI)
	}
	if v.seen == nil {
		v.seen = map[string]bool{}
	}
	v.seen[claims.JTI] = true
	return nil
}

// publicJWK reads a header's jwk: an EC P-256 public key on the curve, with
// no private member.
func publicJWK(m map[string]any) (dpop.JWK, *ecdsa.PublicKey, error) {
	if m == nil {
		return dpop.JWK{}, nil, errors.New("dpop: no jwk")
	}
	if _, ok := m["d"]; ok {
		return dpop.JWK{}, nil, errors.New("dpop: jwk carries the private key d")
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	jwk := dpop.JWK{Kty: str("kty"), Crv: str("crv"), X: str("x"), Y: str("y")}
	if jwk.Kty != "EC" || jwk.Crv != "P-256" {
		return jwk, nil, fmt.Errorf("dpop: jwk is %s/%s, not EC/P-256", jwk.Kty, jwk.Crv)
	}
	x, err := decode(jwk.X)
	if err != nil || len(x) != 32 {
		return jwk, nil, errors.New("dpop: jwk x is not a P-256 coordinate")
	}
	y, err := decode(jwk.Y)
	if err != nil || len(y) != 32 {
		return jwk, nil, errors.New("dpop: jwk y is not a P-256 coordinate")
	}
	point := append(append([]byte{4}, x...), y...)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return jwk, nil, fmt.Errorf("dpop: jwk is not on P-256: %w", err)
	}
	return jwk, pub, nil
}

func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// ProofJKT is the thumbprint of the key a proof's header names, read
// without checking the proof: what a mint binds the token it answers to
// (cnf.jkt), before Verify checks the proof under it.
func ProofJKT(proof string) (string, error) {
	head, _, err := proofParts(proof)
	if err != nil {
		return "", err
	}
	jwk, _, err := publicJWK(head)
	if err != nil {
		return "", err
	}
	return dpop.Thumbprint(jwk), nil
}

// ProofNonce is the nonce a proof's claims carry, "" for none.
func ProofNonce(proof string) (string, error) {
	_, claims, err := proofParts(proof)
	if err != nil {
		return "", err
	}
	n, _ := claims["nonce"].(string)
	return n, nil
}

func proofParts(proof string) (head, claims map[string]any, err error) {
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		return nil, nil, errors.New("dpop: proof is not a compact JWS")
	}
	rawHead, err := decode(parts[0])
	if err != nil {
		return nil, nil, fmt.Errorf("dpop: header: %w", err)
	}
	rawClaims, err := decode(parts[1])
	if err != nil {
		return nil, nil, fmt.Errorf("dpop: claims: %w", err)
	}
	var h struct {
		JWK map[string]any `json:"jwk"`
	}
	if err := json.Unmarshal(rawHead, &h); err != nil {
		return nil, nil, fmt.Errorf("dpop: header: %w", err)
	}
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		return nil, nil, fmt.Errorf("dpop: claims: %w", err)
	}
	return h.JWK, claims, nil
}
