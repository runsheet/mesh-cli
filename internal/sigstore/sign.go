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
	chain, err := s.certificate(ctx, idToken, key)
	if err != nil {
		return "", "", err
	}
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
	entry, err := s.logEnvelope(ctx, raw, leafPEM(chain))
	if err != nil {
		return "", "", err
	}
	return string(raw), entry, nil
}

// certificate asks Fulcio for a certificate of key for the token's
// identity, proving possession by signing the token's subject.
func (s *Signer) certificate(ctx context.Context, idToken string, key *ecdsa.PrivateKey) (string, error) {
	sub, err := subjectOf(idToken)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(sub))
	proof, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
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
		return "", fmt.Errorf("sigstore: fulcio: %w", err)
	}
	var certs []string
	switch {
	case out.Embedded != nil:
		certs = out.Embedded.Chain.Certificates
	case out.Detached != nil:
		certs = out.Detached.Chain.Certificates
	}
	if len(certs) == 0 {
		return "", errors.New("sigstore: fulcio answered no certificate")
	}
	return strings.Join(certs, ""), nil
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
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
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

// leafPEM is the first certificate of a PEM chain.
func leafPEM(chain string) string {
	b, _ := pem.Decode([]byte(chain))
	if b == nil {
		return ""
	}
	return string(pem.EncodeToMemory(b))
}
