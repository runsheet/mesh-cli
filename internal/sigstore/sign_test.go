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
	"errors"
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

// TestFulcioRefusalIsAnsweredAndNamesTheToken: each request goes on a
// connection of its own (Fulcio closes an idle one after 30 s, and a
// POST that reuses it as it closes fails with a bare EOF), and Fulcio's
// refusal comes back as an *HTTPError — permanent for a 400 — whose text
// names the token's aud and iss, never the token, since Fulcio's own
// answer does not say why.
func TestFulcioRefusalIsAnsweredAndNamesTheToken(t *testing.T) {
	var closes []bool
	fulcio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		closes = append(closes, r.Close)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3,"message":"There was an error processing the identity token","details":[]}`))
	}))
	defer fulcio.Close()
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"mesh:acme/p:job:j:lease:1","aud":["mesh-session"],"iss":"https://auth.test"}`)) + ".c2ln"
	s := &Signer{FulcioURL: fulcio.URL, RekorURL: "http://rekor.invalid"}
	for range 2 {
		_, _, err := s.Sign(context.Background(), token, "application/vnd.in-toto+json", []byte(`{}`))
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusBadRequest || !he.Permanent() {
			t.Fatalf("got %v, want Fulcio's 400 as a permanent *HTTPError", err)
		}
		want := `400 Bad Request: {"code":3,"message":"There was an error processing the identity token","details":[]} (the identity token's aud is [mesh-session], iss "https://auth.test")`
		if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), token) {
			t.Fatalf("error %q, want it to say %q and never the token", err, want)
		}
	}
	if len(closes) != 2 || !closes[0] || !closes[1] {
		t.Fatalf("requests asked to close their connection: %v, want both", closes)
	}
	for _, c := range []struct {
		status    int
		permanent bool
	}{{400, true}, {401, true}, {408, false}, {429, false}, {503, false}} {
		if got := (&HTTPError{Status: c.status}).Permanent(); got != c.permanent {
			t.Errorf("%d permanent %v, want %v", c.status, got, c.permanent)
		}
	}
}
