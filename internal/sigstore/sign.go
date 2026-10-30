// Package sigstore signs bytes keylessly with a Sigstore Fulcio and Rekor:
// an ephemeral key, a short-lived certificate for an OIDC identity token,
// a DSSE signature over the payload, and the signature logged in Rekor.
// It knows nothing of what it signs or whose: a product hands the node a
// payload and its type, the node a token naming its session (mesh-cli
// knows nothing of any product, runsheet ADR 077).
package sigstore

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Signer is a Fulcio and a Rekor to sign through.
type Signer struct {
	FulcioURL string
	RekorURL  string
	// HTTP is the client; nil is one with a 30-second timeout.
	HTTP *http.Client
}

// Enabled is whether both services are configured.
func (s *Signer) Enabled() bool {
	return s != nil && strings.TrimSpace(s.FulcioURL) != "" && strings.TrimSpace(s.RekorURL) != ""
}

// Signature is a DSSE envelope's signature: the certificate chain (PEM,
// leaf first) is the signer's identity.
type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
	Cert  string `json:"cert,omitempty"`
}

// Envelope is a DSSE envelope; Payload is standard base64.
type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

// PAE is DSSE's pre-authentication encoding of a payload and its type.
func PAE(payloadType string, payload []byte) []byte {
	var b bytes.Buffer
	b.WriteString("DSSEv1 ")
	b.WriteString(strconv.Itoa(len(payloadType)))
	b.WriteByte(' ')
	b.WriteString(payloadType)
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(len(payload)))
	b.WriteByte(' ')
	b.Write(payload)
	return b.Bytes()
}

// Sign signs payload (of payloadType) as the identity idToken names: it
// answers the envelope (JSON) and Rekor's entry for its signature (JSON,
// as Rekor's API answers it).
func (s *Signer) Sign(ctx context.Context, idToken, payloadType string, payload []byte) (string, string, error) {
	if !s.Enabled() {
		return "", "", errors.New("sigstore: no Fulcio or Rekor configured")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	certs, err := s.certificate(ctx, idToken, key)
	if err != nil {
		return "", "", err
	}
	chain := strings.Join(certs, "")
	digest := sha256.Sum256(PAE(payloadType, payload))
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return "", "", err
	}
	env := Envelope{PayloadType: payloadType, Payload: base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{{Sig: base64.StdEncoding.EncodeToString(sig), Cert: chain}}}
	raw, err := json.Marshal(env)
	if err != nil {
		return "", "", err
	}
	entry, err := s.logEnvelope(ctx, raw, certs[0])
	if err != nil {
		return "", "", err
	}
	return string(raw), entry, nil
}

// certificate asks Fulcio for a certificate of key for the token's
// identity, proving possession by signing the token's subject. It is the
// chain, leaf first, each certificate re-encoded as PEM: Fulcio's
// signingCert answer ends its certificates without a newline (its trust
// bundle does not), so they cannot be joined as they come.
func (s *Signer) certificate(ctx context.Context, idToken string, key *ecdsa.PrivateKey) ([]string, error) {
	sub, err := subjectOf(idToken)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(sub))
	proof, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		return nil, err
	}
	req := map[string]any{
		"credentials": map[string]string{"oidcIdentityToken": idToken},
		"publicKeyRequest": map[string]any{
			"publicKey":         map[string]string{"algorithm": "ECDSA", "content": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))},
			"proofOfPossession": base64.StdEncoding.EncodeToString(proof),
		},
	}
	var out struct {
		Embedded *struct {
			Chain struct{ Certificates []string } `json:"chain"`
		} `json:"signedCertificateEmbeddedSct"`
		Detached *struct {
			Chain struct{ Certificates []string } `json:"chain"`
		} `json:"signedCertificateDetachedSct"`
	}
	if err := s.post(ctx, strings.TrimRight(s.FulcioURL, "/")+"/api/v2/signingCert", req, &out); err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status/100 == 4 {
			// Fulcio's answer only says "There was an error processing
			// the identity token": name what the token claims, so a
			// wrong audience or issuer is plain from the node's side
			return nil, fmt.Errorf("sigstore: fulcio: %w (the identity token's %s)", err, tokenNames(idToken))
		}
		return nil, fmt.Errorf("sigstore: fulcio: %w", err)
	}
	var certs []string
	switch {
	case out.Embedded != nil:
		certs = out.Embedded.Chain.Certificates
	case out.Detached != nil:
		certs = out.Detached.Chain.Certificates
	}
	if len(certs) == 0 {
		return nil, errors.New("sigstore: fulcio answered no certificate")
	}
	chain := make([]string, len(certs))
	for i, c := range certs {
		b, _ := pem.Decode([]byte(strings.TrimSpace(c) + "\n"))
		if b == nil || b.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("sigstore: fulcio's certificate %d is not PEM", i)
		}
		chain[i] = string(pem.EncodeToMemory(b))
	}
	return chain, nil
}

// logEnvelope enters the envelope's signature in Rekor as a dsse entry
// whose verifier is the signing certificate.
func (s *Signer) logEnvelope(ctx context.Context, envelope []byte, leaf string) (string, error) {
	req := map[string]any{
		"apiVersion": "0.0.1",
		"kind":       "dsse",
		"spec": map[string]any{"proposedContent": map[string]any{
			"envelope":  string(envelope),
			"verifiers": []string{base64.StdEncoding.EncodeToString([]byte(leaf))},
		}},
	}
	var out json.RawMessage
	if err := s.post(ctx, strings.TrimRight(s.RekorURL, "/")+"/api/v1/log/entries", req, &out); err != nil {
		return "", fmt.Errorf("sigstore: rekor: %w", err)
	}
	return string(out), nil
}

// HTTPError is Fulcio's or Rekor's answer when it is not a 2xx: its
// status and body.
type HTTPError struct {
	Status int
	Text   string // the status line's text, e.g. "400 Bad Request"
	Body   string
}

func (e *HTTPError) Error() string { return strings.TrimSpace(e.Text + ": " + e.Body) }

// Permanent is whether the answer is a refusal that asking again will
// not change: a 4xx other than 408 (timeout) and 429 (too many).
func (e *HTTPError) Permanent() bool {
	return e.Status/100 == 4 && e.Status != http.StatusRequestTimeout && e.Status != http.StatusTooManyRequests
}

// post sends one request on a connection of its own (Close): a signing
// is rare, and a pooled connection kept between tries is closed by
// Fulcio after its idle-connection-timeout (30 s by default) — a try
// that reuses it as the server closes it fails with a bare EOF, since a
// POST is not replayed.
func (s *Signer) post(ctx context.Context, url string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Close = true
	c := s.HTTP
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return &HTTPError{Status: resp.StatusCode, Text: resp.Status, Body: strings.TrimSpace(string(raw))}
	}
	return json.Unmarshal(raw, out)
}

// tokenNames is what a JWT says of itself that Fulcio checks — its aud
// and iss, unverified — for an error; never the token.
func tokenNames(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "is not a JWT"
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "claims are not base64url"
	}
	var c struct {
		Aud any    `json:"aud"`
		Iss string `json:"iss"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return "claims are not JSON"
	}
	return fmt.Sprintf("aud is %v, iss %q", c.Aud, c.Iss)
}

// subjectOf is a JWT's sub claim, unverified: Fulcio verifies the token.
func subjectOf(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("sigstore: the identity token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("sigstore: the identity token's claims: %w", err)
	}
	var c struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &c); err != nil || c.Sub == "" {
		return "", errors.New("sigstore: the identity token names no subject")
	}
	return c.Sub, nil
}
