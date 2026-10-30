package nodetools

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
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// sessionToken is the lease's signing token: a JWT naming sub for
// audience sigstore, as Latchkey mints it for Fulcio (unsigned: the fake
// Fulcio reads it as Fulcio would after verifying it).
func sessionToken(sub string) string { return idToken(sub, "sigstore") }

// idToken is a session JWT naming sub for aud.
func idToken(sub, aud string) string {
	claims, _ := json.Marshal(map[string]any{"sub": sub, "aud": aud, "iss": "https://latchkey.test"})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

// fulcioRefusal is what Fulcio answers for an identity token it will not
// take, whatever the reason (its log alone says "expected audience").
const fulcioRefusal = `{"code":3,"message":"There was an error processing the identity token","details":[]}`

// fakeFulcio is a test Fulcio: as Fulcio's runsheet-mesh issuer, it
// refuses a token whose aud is not its client id `sigstore` with its
// 400; it takes only want's token, checks the proof of possession over
// the token's subject, and certifies the key for that subject as its
// SAN URI, chained to its own root. down makes it answer 503; drop, after
// down, closes the connection unanswered (a bare EOF to the client).
type fakeFulcio struct {
	srv    *httptest.Server
	caKey  *ecdsa.PrivateKey
	ca     *x509.Certificate
	caPEM  string
	want   string
	mu     sync.Mutex
	down   int // answer 503 this many more times; -1 for ever
	drop   bool
	issued *x509.Certificate
	calls  int
}

func newFakeFulcio(t *testing.T, token string) *fakeFulcio {
	f := &fakeFulcio{want: token}
	f.caKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test fulcio"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &f.caKey.PublicKey, f.caKey)
	f.ca, _ = x509.ParseCertificate(der)
	f.caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFulcio) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.down != 0 {
		if f.down > 0 {
			f.down--
		}
		http.Error(w, "fulcio is down", http.StatusServiceUnavailable)
		return
	}
	if f.drop {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	var in struct {
		Credentials      struct{ OidcIdentityToken string }
		PublicKeyRequest struct {
			PublicKey         struct{ Content string }
			ProofOfPossession string
		}
	}
	json.NewDecoder(r.Body).Decode(&in)
	if p := strings.Split(in.Credentials.OidcIdentityToken, "."); len(p) == 3 {
		raw, _ := base64.RawURLEncoding.DecodeString(p[1])
		var c struct{ Aud string }
		json.Unmarshal(raw, &c)
		if c.Aud != "sigstore" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, fulcioRefusal)
			return
		}
	}
	if in.Credentials.OidcIdentityToken != f.want {
		http.Error(w, "unknown token", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(f.want, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct{ Sub string }
	json.Unmarshal(raw, &c)
	b, _ := pem.Decode([]byte(in.PublicKeyRequest.PublicKey.Content))
	pub, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		http.Error(w, "key", http.StatusBadRequest)
		return
	}
	proof, _ := base64.StdEncoding.DecodeString(in.PublicKeyRequest.ProofOfPossession)
	sum := sha256.Sum256([]byte(c.Sub))
	if !ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), sum[:], proof) {
		http.Error(w, "proof of possession", http.StatusBadRequest)
		return
	}
	u, _ := url.Parse(c.Sub)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now(), NotAfter: time.Now().Add(10 * time.Minute),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}, URIs: []*url.URL{u}}
	der, _ := x509.CreateCertificate(rand.Reader, leaf, f.ca, pub, f.caKey)
	f.issued, _ = x509.ParseCertificate(der)
	// as dl360's Fulcio answers: detached SCT, no trailing newlines
	certs := []string{strings.TrimSuffix(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), "\n"), strings.TrimSuffix(f.caPEM, "\n")}
	json.NewEncoder(w).Encode(map[string]any{"signedCertificateDetachedSct": map[string]any{"chain": map[string]any{"certificates": certs}}})
}

// fakeRekor is a test Rekor: it keeps each dsse entry and answers one
// entry under a uuid.
type fakeRekor struct {
	srv     *httptest.Server
	mu      sync.Mutex
	entries []map[string]any
}

func newFakeRekor(t *testing.T) *fakeRekor {
	k := &fakeRekor{}
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e map[string]any
		json.NewDecoder(r.Body).Decode(&e)
		k.mu.Lock()
		k.entries = append(k.entries, e)
		n := len(k.entries)
		k.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"24296fb2%056d":{"logIndex":%d,"integratedTime":%d,"body":"e30="}}`, n, n-1, time.Now().Unix())
	}))
	t.Cleanup(k.srv.Close)
	return k
}

// fakeMesh is the Poster: it keeps blobs and records, and names each
// record by its statement's digest as Mesh does (wrongID names them
// otherwise).
type fakeMesh struct {
	mu      sync.Mutex
	blobs   map[string][]byte
	records []meshapi.EvidencePost
	order   []string
	wrongID bool
}

func (m *fakeMesh) PutBlob(_ context.Context, digest string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if Digest(data) != digest {
		return fmt.Errorf("the bytes are not %s", digest)
	}
	if m.blobs == nil {
		m.blobs = map[string][]byte{}
	}
	m.blobs[digest] = data
	m.order = append(m.order, "blob")
	return nil
}

func (m *fakeMesh) Evidence(_ context.Context, _ string, recs []meshapi.EvidencePost) (*meshapi.EvidenceResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, r := range recs {
		var env struct{ Payload string }
		json.Unmarshal(r.Envelope, &env)
		p, _ := base64.StdEncoding.DecodeString(env.Payload)
		id := Digest(p)
		if m.wrongID {
			id = "sha256:00"
		}
		ids = append(ids, id)
	}
	m.records = append(m.records, recs...)
	m.order = append(m.order, "evidence")
	return &meshapi.EvidenceResponse{IDs: ids}, nil
}

// statementOf is a posted record's statement.
func statementOf(t *testing.T, post meshapi.EvidencePost) (statement, []byte) {
	t.Helper()
	var env struct{ Payload string }
	if err := json.Unmarshal(post.Envelope, &env); err != nil {
		t.Fatal(err)
	}
	p, _ := base64.StdEncoding.DecodeString(env.Payload)
	var st statement
	if err := json.Unmarshal(p, &st); err != nil {
		t.Fatal(err)
	}
	return st, p
}
