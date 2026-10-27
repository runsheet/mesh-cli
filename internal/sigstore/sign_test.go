package sigstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSignIsAVerifiableDSSEEnvelopeLoggedInRekor: Sign asks Fulcio for a
// certificate of an ephemeral key (proving possession over the token's
// subject), signs PAE(type, payload) with it, and logs a dsse entry whose
// verifier is that certificate; the envelope verifies with the
// certificate's key and carries the chain. It holds for both of Fulcio's
// answers: the embedded-SCT one with newline-ended certificates, and the
// detached one dl360's Fulcio (v1.6.6, no CT log) gives, whose
// certificates end at "-----END CERTIFICATE-----" with no newline.
func TestSignIsAVerifiableDSSEEnvelopeLoggedInRekor(t *testing.T) {
	for _, shape := range []struct {
		name, field string
		trim        bool
	}{
		{"embedded SCT", "signedCertificateEmbeddedSct", false},
		{"detached SCT, as dl360's Fulcio answers", "signedCertificateDetachedSct", true},
	} {
		t.Run(shape.name, func(t *testing.T) { signsWithFulcioAnswering(t, shape.field, shape.trim) })
	}
}

func signsWithFulcioAnswering(t *testing.T, field string, trim bool) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	ca, _ = x509.ParseCertificate(caDER)

	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"mesh:acme/p:goal:g:item:1"}`)) + ".sig"
	var issued *x509.Certificate
	var logged map[string]any
	fulcio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Credentials      struct{ OidcIdentityToken string }
			PublicKeyRequest struct {
				PublicKey         struct{ Content string }
				ProofOfPossession string
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Credentials.OidcIdentityToken != token {
			http.Error(w, "token", 400)
			return
		}
		b, _ := pem.Decode([]byte(in.PublicKeyRequest.PublicKey.Content))
		pub, _ := x509.ParsePKIXPublicKey(b.Bytes)
		proof, _ := base64.StdEncoding.DecodeString(in.PublicKeyRequest.ProofOfPossession)
		sum := sha256.Sum256([]byte("mesh:acme/p:goal:g:item:1"))
		if !ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), sum[:], proof) {
			http.Error(w, "proof", 400)
			return
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now(), NotAfter: time.Now().Add(10 * time.Minute)}
		der, _ := x509.CreateCertificate(rand.Reader, leaf, ca, pub, caKey)
		issued, _ = x509.ParseCertificate(der)
		certs := []string{string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))}
		if trim {
			for i := range certs {
				certs[i] = strings.TrimSuffix(certs[i], "\n")
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{field: map[string]any{"chain": map[string]any{"certificates": certs}}})
	}))
	defer fulcio.Close()
	rekor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&logged)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"abc":{"logIndex":0}}`))
	}))
	defer rekor.Close()

	s := &Signer{FulcioURL: fulcio.URL, RekorURL: rekor.URL}
	env, entry, err := s.Sign(context.Background(), token, "application/vnd.in-toto+json", []byte(`{"_type":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if entry != `{"abc":{"logIndex":0}}` {
		t.Fatalf("the entry is Rekor's answer as it is, got %s", entry)
	}
	var e Envelope
	if err := json.Unmarshal([]byte(env), &e); err != nil || len(e.Signatures) != 1 {
		t.Fatalf("envelope: %v %s", err, env)
	}
	sig, _ := base64.StdEncoding.DecodeString(e.Signatures[0].Sig)
	payload, _ := base64.StdEncoding.DecodeString(e.Payload)
	digest := sha256.Sum256(PAE(e.PayloadType, payload))
	if !ecdsa.VerifyASN1(issued.PublicKey.(*ecdsa.PublicKey), digest[:], sig) {
		t.Fatal("the signature does not verify with the issued certificate's key")
	}
	rest := []byte(e.Signatures[0].Cert)
	for _, want := range [][]byte{issued.Raw, caDER} {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil || string(b.Bytes) != string(want) {
			t.Fatalf("the envelope's chain is not leaf then CA as PEM: %q", e.Signatures[0].Cert)
		}
	}
	if logged["kind"] != "dsse" {
		t.Fatalf("logged a %v entry, want dsse", logged["kind"])
	}
	content := logged["spec"].(map[string]any)["proposedContent"].(map[string]any)
	if content["envelope"] != env {
		t.Fatal("Rekor was not sent the envelope as signed")
	}
	v, _ := base64.StdEncoding.DecodeString(content["verifiers"].([]any)[0].(string))
	if b, _ := pem.Decode(v); b == nil || string(b.Bytes) != string(issued.Raw) {
		t.Fatal("Rekor's verifier is not the signing certificate")
	}
	if _, _, err := (&Signer{}).Sign(context.Background(), token, "t", nil); err == nil {
		t.Fatal("an unconfigured signer signed")
	}
}
