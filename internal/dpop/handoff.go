package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"errors"
	"fmt"
)

// A sandboxed lease's relay runs in a sidecar in the lease's pod
// (projectmesh-io/api ADR 001 amendment "relay sidecar"), and signs the
// lease's DPoP proofs there. The lease's key is lease-scoped, so it is
// the one key that may leave the node's process: written once, by the
// node, into the credential file only the sidecar reads. Handoff is that
// one way out, and KeyFromHandoff the one way back in. Nothing else
// exports the private scalar, and a Handoff prints nothing private.

// Handoff is a key's private half for the relay sidecar's credential
// file: the public JWK and the private scalar D, base64url.
type Handoff struct {
	JWK JWK    `json:"jwk"`
	D   string `json:"d"`
}

// String names the key by its thumbprint alone.
func (h Handoff) String() string { return "dpop.Handoff(jkt=" + Thumbprint(h.JWK) + ")" }

// GoString is String.
func (h Handoff) GoString() string { return h.String() }

// Format prints String for every verb.
func (h Handoff) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(h.String())) }

// Handoff answers the key's private half, for the relay sidecar's
// credential file alone.
func (k *Key) Handoff() (Handoff, error) {
	if k == nil || k.priv == nil {
		return Handoff{}, errors.New("dpop: handoff: no key")
	}
	d, err := k.priv.Bytes()
	if err != nil {
		return Handoff{}, fmt.Errorf("dpop: handoff: %w", err)
	}
	return Handoff{JWK: k.jwk, D: b64(d)}, nil
}

// KeyFromHandoff is the key a Handoff carries. It refuses one whose
// private scalar is not the public key's.
func KeyFromHandoff(h Handoff) (*Key, error) {
	if h.JWK.Kty != "EC" || h.JWK.Crv != "P-256" {
		return nil, errors.New("dpop: handoff: not a P-256 key")
	}
	d, err := base64.RawURLEncoding.DecodeString(h.D)
	if err != nil || len(d) != coordSize {
		return nil, errors.New("dpop: handoff: the private scalar is not 32 bytes of base64url")
	}
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d)
	if err != nil {
		return nil, fmt.Errorf("dpop: handoff: %w", err)
	}
	pub, err := priv.PublicKey.Bytes()
	if err != nil {
		return nil, fmt.Errorf("dpop: handoff: %w", err)
	}
	jwk := JWK{Kty: "EC", Crv: "P-256", X: b64(pub[1 : 1+coordSize]), Y: b64(pub[1+coordSize:])}
	if jwk != h.JWK {
		return nil, errors.New("dpop: handoff: the private scalar is not the JWK's")
	}
	return &Key{priv: priv, jwk: jwk, jkt: Thumbprint(jwk)}, nil
}
