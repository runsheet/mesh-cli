package results

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/dpop/dpoptest"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// session is a lease's Latchkey session: its key, and the token of the
// moment, which a test swaps as a refresh would.
type session struct {
	*dpop.Key
	mu    sync.Mutex
	token string
}

func (s *session) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, nil
}

func (s *session) set(tok string) {
	s.mu.Lock()
	s.token = tok
	s.mu.Unlock()
}

func newSession(t *testing.T) *session {
	k, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return &session{Key: k}
}

// product is the offer's results location: a PUT is taken only on
// `Authorization: DPoP <token>` with one DPoP proof of this PUT — htm,
// htu the URL without its query, ath of the token, a jti never seen —
// by the key the token is bound to; else 401 with a DPoP
// WWW-Authenticate.
type product struct {
	srv *httptest.Server
	v   dpoptest.Verifier

	mu      sync.Mutex
	tokens  map[string]tokenOf
	puts    []put
	tries   int
	fail500 int // answer 500 this many more times before taking the PUT
	nonce   string
}

type tokenOf struct{ jkt string }

type put struct {
	header http.Header
	body   []byte
}

func newProduct(t *testing.T) *product {
	p := &product{tokens: map[string]tokenOf{}}
	p.srv = httptest.NewServer(p)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *product) mint(s *session) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	tok := fmt.Sprintf("mst_%d", len(p.tokens)+1)
	p.tokens[tok] = tokenOf{jkt: s.Thumbprint()}
	return tok
}

func (p *product) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tries++
	refuse := func(why string) {
		w.Header().Set("WWW-Authenticate", `DPoP realm="mesh", algs="ES256", error="invalid_token", error_description="`+why+`"`)
		if p.nonce != "" {
			w.Header().Set("DPoP-Nonce", p.nonce)
		}
		http.Error(w, why, http.StatusUnauthorized)
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "DPoP ")
	proofs := r.Header.Values("DPoP")
	if !ok || tok == "" || len(proofs) != 1 {
		refuse("a PUT is sent with the lease's mesh-session token")
		return
	}
	of, known := p.tokens[tok]
	if !known {
		refuse("not a session token")
		return
	}
	if err := p.v.Verify(proofs[0], dpoptest.Request{Method: r.Method, URL: "http://" + r.Host + r.URL.EscapedPath(), Token: tok, JKT: of.jkt}); err != nil {
		refuse(err.Error())
		return
	}
	if p.nonce != "" {
		got, err := dpoptest.ProofNonce(proofs[0])
		if err != nil || got != p.nonce {
			refuse("use the nonce")
			return
		}
	}
	if p.fail500 > 0 {
		p.fail500--
		http.Error(w, "try again", http.StatusInternalServerError)
		return
	}
	p.puts = append(p.puts, put{header: r.Header.Clone(), body: b})
	w.WriteHeader(http.StatusNoContent)
}

func (p *product) loc() meshapi.ResultsLocation {
	return meshapi.ResultsLocation{UploadURL: p.srv.URL + "/mesh/v1/results/run-1?x=1#f", Headers: map[string]string{"X-Product": "p1"}, ExpiresAt: time.Now().Add(time.Hour)}
}

// The server receives the complete input bytes byte for byte, with the
// location's own headers and a DPoP-authenticated request; the
// returned digest and byte count match the input.
func TestUploadSendsTheCompleteBodyOnADPoPAuthenticatedPUT(t *testing.T) {
	p := newProduct(t)
	s := newSession(t)
	s.set(p.mint(s))
	body := []byte(strings.Repeat("the complete raw test output\n", 50))
	res, err := Upload(context.Background(), p.loc(), body, Options{Session: s})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Bytes != int64(len(body)) {
		t.Fatalf("bytes %d, want %d", res.Bytes, len(body))
	}
	sum := sha256Hex(body)
	if res.Digest != "sha256:"+sum {
		t.Fatalf("digest %s, want sha256:%s", res.Digest, sum)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.puts) != 1 {
		t.Fatalf("the product saw %d puts", len(p.puts))
	}
	if string(p.puts[0].body) != string(body) {
		t.Fatalf("the product's body differs from the input")
	}
	if p.puts[0].header.Get("X-Product") != "p1" {
		t.Fatalf("the location's own headers did not ride: %v", p.puts[0].header)
	}
}

// A transient 500 on the first attempt is retried per Options.Tries
// before succeeding.
func TestUploadRetriesATransient500(t *testing.T) {
	p := newProduct(t)
	p.fail500 = 1
	s := newSession(t)
	s.set(p.mint(s))
	body := []byte("test output")
	res, err := Upload(context.Background(), p.loc(), body, Options{Session: s, Tries: 3, Backoff: time.Millisecond})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Bytes != int64(len(body)) {
		t.Fatalf("bytes %d", res.Bytes)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tries != 2 {
		t.Fatalf("the product was asked %d times, want 2", p.tries)
	}
	if len(p.puts) != 1 {
		t.Fatalf("the product kept %d puts, want 1", len(p.puts))
	}
}

// A PUT past Options.Tries without landing is dropped.
func TestUploadDroppedAfterItsTries(t *testing.T) {
	p := newProduct(t)
	p.fail500 = 5
	s := newSession(t)
	s.set(p.mint(s))
	_, err := Upload(context.Background(), p.loc(), []byte("x"), Options{Session: s, Tries: 2, Backoff: time.Millisecond})
	if err == nil {
		t.Fatal("want an error")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tries != 2 {
		t.Fatalf("the product was asked %d times, want 2", p.tries)
	}
	if len(p.puts) != 0 {
		t.Fatalf("the product kept %d puts, want 0", len(p.puts))
	}
}

// A 401 that names a DPoP-Nonce is sent again at once on a proof that
// carries it, and lands.
func TestUploadA401WithANonceIsSentAgainWithIt(t *testing.T) {
	p := newProduct(t)
	p.nonce = "n-123"
	s := newSession(t)
	s.set(p.mint(s))
	res, err := Upload(context.Background(), p.loc(), []byte("x"), Options{Session: s, Tries: 1})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Bytes != 1 {
		t.Fatalf("bytes %d", res.Bytes)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tries != 2 {
		t.Fatalf("the product was asked %d times", p.tries)
	}
}

// A PUT without the session token, or on another token the product
// does not know, is refused and not retried past the second 401.
func TestUploadRefusedTwiceIsDroppedNotRetriedFurther(t *testing.T) {
	p := newProduct(t)
	plain := Options{Tries: 3, Backoff: time.Millisecond}
	_, err := Upload(context.Background(), p.loc(), []byte("x"), plain)
	if err == nil {
		t.Fatal("want an error")
	}
	p.mu.Lock()
	tries := p.tries
	puts := len(p.puts)
	p.mu.Unlock()
	if tries != 1 || puts != 0 {
		t.Fatalf("the product was asked %d times, kept %d", tries, puts)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
