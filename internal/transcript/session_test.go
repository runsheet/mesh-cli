package transcript

import (
	"context"
	"encoding/base64"
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

// product is runsheet's transcript location (gateway/meshv1stream.go):
// a chunk is taken only on `Authorization: DPoP <token>` with one DPoP
// proof of this PUT — htm, htu the URL without its query, ath of the
// token, a jti never seen — by the key the token is bound to, and the
// token's lease is the chunk's Mesh-Lease; else 401 with a DPoP
// WWW-Authenticate.
type product struct {
	srv *httptest.Server
	v   dpoptest.Verifier

	mu     sync.Mutex
	tokens map[string]tokenOf // token → its jkt and lease
	chunks []chunk
	tokUse []string // the token each kept chunk rode on
	tries  int
	// nonce, when set, is asked for on every proof that does not carry it
	nonce string
}

type tokenOf struct{ jkt, lease string }

func newProduct(t *testing.T) *product {
	p := &product{tokens: map[string]tokenOf{}}
	p.srv = httptest.NewServer(p)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *product) mint(s *session, lease string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	tok := fmt.Sprintf("mst_%d", len(p.tokens)+1)
	p.tokens[tok] = tokenOf{jkt: s.Thumbprint(), lease: lease}
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
		refuse("a chunk is sent with the lease's mesh-session token")
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
	if p.nonce != "" && proofNonce(proofs[0]) != p.nonce {
		refuse("use the nonce")
		return
	}
	if of.lease != r.Header.Get(HeaderLease) {
		refuse("the token's lease is not the chunk's Mesh-Lease")
		return
	}
	p.chunks = append(p.chunks, chunk{r.Header.Clone(), string(b)})
	p.tokUse = append(p.tokUse, tok)
	w.WriteHeader(http.StatusNoContent)
}

func proofNonce(proof string) string {
	parts := strings.Split(proof, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_, after, ok := strings.Cut(string(raw), `"nonce":"`)
	if !ok {
		return ""
	}
	n, _, _ := strings.Cut(after, `"`)
	return n
}

func (p *product) loc() meshapi.Transcript {
	return meshapi.Transcript{UploadURL: p.srv.URL + "/mesh/v1/transcripts/rec-1?x=1#f", Headers: map[string]string{"X-Product": "p1"}, ExpiresAt: time.Now().Add(time.Hour)}
}

func newSession(t *testing.T) *session {
	k, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return &session{Key: k}
}

// Every chunk rides on the lease's session token with a fresh proof of
// its PUT (mesh-specs gap 30): the product verifies each, the offer's
// headers still go, and a token refreshed mid-lease carries the next
// chunk; the token and proof are never logged.
func TestChunksGoOnTheLeasesSessionToken(t *testing.T) {
	p := newProduct(t)
	s := newSession(t)
	first := p.mint(s, "lease-1")
	s.set(first)
	var logs []string
	u := New(p.loc(), "lease-1", nil, Options{Every: time.Hour, Session: s, Backoff: time.Millisecond,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	u.Line([]byte(`{"type":"system","subtype":"init"}`))
	u.send(false)
	second := p.mint(s, "lease-1")
	s.set(second)
	u.Line([]byte(`{"type":"result","subtype":"success"}`))
	up := u.Close()
	if up.Chunks != 2 || up.Failed != 0 {
		t.Fatalf("upload %+v, logs %v", up, logs)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.chunks) != 2 || p.tokUse[0] != first || p.tokUse[1] != second {
		t.Fatalf("the chunks rode on %v", p.tokUse)
	}
	for _, c := range p.chunks {
		if c.header.Get("X-Product") != "p1" || c.header.Get(HeaderLease) != "lease-1" {
			t.Fatalf("chunk headers %v", c.header)
		}
	}
	if p.chunks[1].header.Get(HeaderFinal) != "true" {
		t.Fatal("the last chunk is not final")
	}
	for _, l := range logs {
		if strings.Contains(l, "mst_") || strings.Contains(l, "eyJ") {
			t.Fatalf("a log carries a credential: %s", l)
		}
	}
}

// A chunk without the session token, or on another lease's token, is
// refused; the node's chunk on another lease's token is retried once on
// a fresh proof, then counted failed with no further tries.
func TestAChunkOnAnotherLeasesTokenIsRefusedAndCountedFailed(t *testing.T) {
	p := newProduct(t)
	plain := New(p.loc(), "lease-1", nil, Options{Every: time.Hour, Tries: 1})
	plain.Line([]byte(`{"type":"system"}`))
	if up := plain.Close(); up.Chunks != 0 || up.Failed != 1 {
		t.Fatalf("a plain chunk: %+v", up)
	}
	s := newSession(t)
	s.set(p.mint(s, "lease-2"))
	p.mu.Lock()
	p.tries = 0
	p.mu.Unlock()
	var logs []string
	u := New(p.loc(), "lease-1", nil, Options{Every: time.Hour, Session: s, Tries: 3, Backoff: time.Millisecond,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	u.Line([]byte(`{"type":"system"}`))
	if up := u.Close(); up.Chunks != 0 || up.Failed != 1 {
		t.Fatalf("upload %+v", up)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tries != 2 || len(p.chunks) != 0 {
		t.Fatalf("the product was asked %d times, kept %d", p.tries, len(p.chunks))
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "refused the lease's session token") || strings.Contains(logs[0], "mst_") {
		t.Fatalf("logs %v", logs)
	}
}

// A 401 that names a DPoP-Nonce is sent again at once on a proof that
// carries it, and lands.
func TestA401WithANonceIsSentAgainWithIt(t *testing.T) {
	p := newProduct(t)
	p.nonce = "n-123"
	s := newSession(t)
	s.set(p.mint(s, "lease-1"))
	u := New(p.loc(), "lease-1", nil, Options{Every: time.Hour, Session: s, Tries: 1})
	u.Line([]byte(`{"type":"system"}`))
	if up := u.Close(); up.Chunks != 1 || up.Failed != 0 {
		t.Fatalf("upload %+v", up)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tries != 2 {
		t.Fatalf("the product was asked %d times", p.tries)
	}
}
