package release

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
)

// fakeFulcio stands in for Fulcio's /api/v2/signingCert: it certifies the
// public key the request carries under an in-process root, so the bundle's
// certificate is the ephemeral key's own. It records the bearer it was sent.
func fakeFulcio(t *testing.T, bearer *atomic.Value) *httptest.Server {
	t.Helper()
	root, rootKey, err := ca.GenerateRootCa()
	if err != nil {
		t.Fatalf("root ca: %v", err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/signingCert" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		bearer.Store(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		var req struct {
			PublicKeyRequest struct {
				PublicKey struct {
					Content string `json:"content"`
				} `json:"publicKey"`
			} `json:"publicKeyRequest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		block, _ := pem.Decode([]byte(req.PublicKeyRequest.PublicKey.Content))
		if block == nil {
			http.Error(w, "no public key", http.StatusBadRequest)
			return
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		leaf := &x509.Certificate{
			SerialNumber:   big.NewInt(2),
			EmailAddresses: []string{"release@runsheet.dev"},
			NotBefore:      time.Now().Add(-time.Minute),
			NotAfter:       time.Now().Add(10 * time.Minute),
			KeyUsage:       x509.KeyUsageDigitalSignature,
			ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, root, pub, rootKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		chain := []string{
			string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"signedCertificateEmbeddedSct": map[string]any{
				"chain": map[string]any{"certificates": chain},
			},
		})
	}))
}

// fakeRekor stands in for Rekor v1's /api/v1/log/entries: it logs the
// proposed entry as sent and answers 201 with one entry at index 0.
func fakeRekor(t *testing.T) *httptest.Server {
	t.Helper()
	logID := hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
	rootHash := hex.EncodeToString(bytes.Repeat([]byte{0xcd}, 32))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/log/entries" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		const uuid = "fake-entry"
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", uuid)
		w.Header().Set("Location", "/api/v1/log/entries/"+uuid)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			uuid: map[string]any{
				"body":           base64.StdEncoding.EncodeToString(body),
				"integratedTime": time.Now().Unix(),
				"logID":          logID,
				"logIndex":       0,
				"verification": map[string]any{
					"inclusionProof": map[string]any{
						"checkpoint": "fake checkpoint",
						"hashes":     []string{},
						"logIndex":   0,
						"rootHash":   rootHash,
						"treeSize":   1,
					},
				},
			},
		})
	}))
}

// A token Fulcio's client can read a subject from; the fake never checks it.
const testIDToken = "eyJhbGciOiJub25lIn0.eyJzdWIiOiJyZWxlYXNlQHJ1bnNoZWV0LmRldiJ9.sig" // #nosec G101

func TestSignBundlesSHA256OfSums(t *testing.T) {
	var bearer atomic.Value
	fulcio := fakeFulcio(t, &bearer)
	defer fulcio.Close()
	rekor := fakeRekor(t)
	defer rekor.Close()

	sums := []byte("0123abcd  mesh_linux_amd64.tar.gz\n4567ef01  mesh_darwin_arm64.tar.gz\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := Sign(ctx, sums, testIDToken, SignOptions{
		FulcioURL: fulcio.URL,
		RekorURL:  rekor.URL,
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if got := bearer.Load(); got != testIDToken {
		t.Errorf("Fulcio was sent bearer %q, want the signing token", got)
	}

	var b bundle.Bundle
	if err := b.UnmarshalJSON(out); err != nil {
		t.Fatalf("parse bundle: %v\n%s", err, out)
	}
	ms := b.GetMessageSignature()
	if ms == nil || ms.GetMessageDigest() == nil {
		t.Fatalf("bundle has no message digest: %s", out)
	}
	want := sha256.Sum256(sums)
	if got := ms.GetMessageDigest().GetDigest(); !bytes.Equal(got, want[:]) {
		t.Errorf("message digest %x, want sha256 of the sums %x", got, want)
	}
	if alg := ms.GetMessageDigest().GetAlgorithm().String(); alg != "SHA2_256" {
		t.Errorf("digest algorithm %s, want SHA2_256", alg)
	}
	if n := len(b.GetVerificationMaterial().GetTlogEntries()); n != 1 {
		t.Errorf("bundle has %d tlog entries, want 1", n)
	}
	if b.GetVerificationMaterial().GetCertificate() == nil {
		t.Error("bundle has no signing certificate")
	}
}

func TestSignRefusesEmptyInput(t *testing.T) {
	if _, err := Sign(context.Background(), nil, testIDToken, SignOptions{}); err == nil {
		t.Error("Sign with no sums: want an error")
	}
	if _, err := Sign(context.Background(), []byte("x"), "", SignOptions{}); err == nil {
		t.Error("Sign with no token: want an error")
	}
}
