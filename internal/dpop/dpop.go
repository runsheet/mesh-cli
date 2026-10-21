// Package dpop is mesh-cli's half of runsheet ADR 064's proof: a session's
// P-256 key, held only in memory, that signs one RFC 9449 DPoP proof per
// request to /mcp, and the RFC 7638 thumbprint its token is bound to.
//
// The key never leaves the process: there is no file, no export of the
// private scalar, and every way the fmt, encoding/json and log/slog
// packages have of printing a Key prints its thumbprint alone.
package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

// Typ and Alg are the proof header's fixed members (ADR 064 §2).
const (
	Typ = "dpop+jwt"
	Alg = "ES256"
)

// coordSize is a P-256 coordinate's and scalar's length in bytes.
const coordSize = 32

// Key is a session's DPoP key. Its zero value is not usable; make one with
// NewKey.
type Key struct {
	priv *ecdsa.PrivateKey
	jwk  JWK
	jkt  string

	// Now is the clock a proof's iat is read from; nil is time.Now.
	Now func() time.Time
}

// JWK is a P-256 public key as a JSON Web Key. It has no member for the
// private scalar, so a proof's header cannot carry one.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// Header is a proof's JOSE header.
type Header struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
	JWK JWK    `json:"jwk"`
}

// Claims are a proof's claims. Ath is left out for the mint request, which
// carries no token yet (ADR 064 §4).
type Claims struct {
	JTI string `json:"jti"`
	HTM string `json:"htm"`
	HTU string `json:"htu"`
	IAT int64  `json:"iat"`
	Ath string `json:"ath,omitempty"`
	// Nonce is the server's DPoP-Nonce, when it asked for one (RFC 9449
	// §8; Latchkey's session_dpop_nonce required).
	Nonce string `json:"nonce,omitempty"`
}

// NewKey makes a fresh P-256 key, held only in memory.
func NewKey() (*Key, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("dpop: generate key: %w", err)
	}
	pub, err := priv.PublicKey.Bytes()
	if err != nil {
		return nil, fmt.Errorf("dpop: public key: %w", err)
	}
	// pub is the uncompressed point, 0x04 || X || Y.
	jwk := JWK{
		Kty: "EC",
		Crv: "P-256",
		X:   b64(pub[1 : 1+coordSize]),
		Y:   b64(pub[1+coordSize:]),
	}
	return &Key{priv: priv, jwk: jwk, jkt: Thumbprint(jwk)}, nil
}

// PublicJWK answers the key's public half as a JWK.
func (k *Key) PublicJWK() JWK { return k.jwk }

// Thumbprint answers the key's RFC 7638 SHA-256 JWK thumbprint, base64url
// without padding: the jkt a bound token names.
func (k *Key) Thumbprint() string { return k.jkt }

// Thumbprint answers an EC JWK's RFC 7638 SHA-256 thumbprint: the hash of
// its required members, crv, kty, x and y, in lexical order with no
// whitespace.
func Thumbprint(j JWK) string {
	// encoding/json writes struct fields in declaration order; this one is
	// declared in the order RFC 7638 asks for.
	canon, _ := json.Marshal(struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{j.Crv, j.Kty, j.X, j.Y})
	sum := sha256.Sum256(canon)
	return b64(sum[:])
}

// Ath answers a token's access-token hash: base64url(sha256(token)).
func Ath(token string) string {
	sum := sha256.Sum256([]byte(token))
	return b64(sum[:])
}

// HTU answers rawURL as a proof's htu: the URL without its query or
// fragment. It refuses a URL that is not absolute.
func HTU(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("dpop: htu: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("dpop: htu: %q is not an absolute URL", rawURL)
	}
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	u.User = nil
	return u.String(), nil
}

// Proof answers a compact ES256 JWS proving possession of the key for one
// request: method and rawURL are the request's, token the access token it
// carries — empty for the mint request, whose proof has no ath. Every call
// has a fresh jti.
func (k *Key) Proof(method, rawURL, token string) (string, error) {
	return k.ProofWithNonce(method, rawURL, token, "")
}

// ProofWithNonce is Proof carrying the server's nonce, "" for none.
func (k *Key) ProofWithNonce(method, rawURL, token, nonce string) (string, error) {
	if k == nil || k.priv == nil {
		return "", errors.New("dpop: proof: no key")
	}
	if method == "" {
		return "", errors.New("dpop: proof: no method")
	}
	htu, err := HTU(rawURL)
	if err != nil {
		return "", err
	}
	jti, err := newJTI()
	if err != nil {
		return "", err
	}
	now := time.Now
	if k.Now != nil {
		now = k.Now
	}
	claims := Claims{JTI: jti, HTM: strings.ToUpper(method), HTU: htu, IAT: now().Unix()}
	if token != "" {
		claims.Ath = Ath(token)
	}
	claims.Nonce = nonce
	head, err := json.Marshal(Header{Typ: Typ, Alg: Alg, JWK: k.jwk})
	if err != nil {
		return "", fmt.Errorf("dpop: proof: header: %w", err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("dpop: proof: claims: %w", err)
	}
	input := b64(head) + "." + b64(body)
	sum := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, sum[:])
	if err != nil {
		return "", fmt.Errorf("dpop: proof: sign: %w", err)
	}
	// ES256 is the fixed 64-byte r || s form (RFC 7518 §3.4), not ASN.1.
	sig := make([]byte, 2*coordSize)
	r.FillBytes(sig[:coordSize])
	s.FillBytes(sig[coordSize:])
	return input + "." + b64(sig), nil
}

// String names the key by its thumbprint; nothing private is printed.
func (k Key) String() string { return "dpop.Key(jkt=" + k.jkt + ")" }

// GoString is String, so %#v prints no private material either.
func (k Key) GoString() string { return k.String() }

// Format prints the key as String for every verb.
func (k Key) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(k.String())) }

// MarshalJSON writes the thumbprint alone.
func (k Key) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		JKT string `json:"jkt"`
	}{k.jkt})
}

// LogValue logs the thumbprint alone.
func (k Key) LogValue() slog.Value { return slog.StringValue(k.String()) }

func newJTI() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("dpop: jti: %w", err)
	}
	return b64(b[:]), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
