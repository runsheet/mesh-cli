package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// receiver is a product's transcript location: it keeps every chunk.
type receiver struct {
	mu     sync.Mutex
	chunks []chunk
	status int
}

type chunk struct {
	header http.Header
	body   string
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.status != 0 {
		w.WriteHeader(rc.status)
		return
	}
	rc.chunks = append(rc.chunks, chunk{r.Header.Clone(), string(b)})
	w.WriteHeader(http.StatusNoContent)
}

func (rc *receiver) all() string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var b strings.Builder
	for _, c := range rc.chunks {
		b.WriteString(c.body)
	}
	return b.String()
}

func start(t *testing.T, status int) (*receiver, meshapi.Transcript) {
	rc := &receiver{status: status}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	return rc, meshapi.Transcript{UploadURL: srv.URL + "/t/job-1?token=product-made", Headers: map[string]string{"X-Product": "p1"}, ExpiresAt: time.Now().Add(time.Hour)}
}

// TestLeaseSecretsNeverReachTheUpload (adr-001-amendment-learnings-and-
// transcripts.md, "the lease's own injected secrets first"): the lease's
// session credential, session token, minted git token and model
// credential — one minted while the session ran — never reach the
// product's location, whether a line carries them plainly, JSON-escaped,
// as a key, or in text that is not JSON; the secret patterns are caught
// too; what is not secret arrives as the session printed it.
func TestLeaseSecretsNeverReachTheUpload(t *testing.T) {
	rc, loc := start(t, 0)
	lksr := "lksr_RequestCredential0123456789"
	session := "msess.opaque.SessionToken.abcdefghijklmnop"
	model := "model-credential-plain-0123456789abcdef"
	git := "relay-minted-git-token-9876543210zyxwv"
	pattern := "ghp_PatternOnlyToken0123456789abcd"
	var mu sync.Mutex
	held := []string{lksr, session, model}
	u := New(loc, "lease-1", func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), held...) }, Options{Every: time.Hour})
	u.Line([]byte(`{"type":"system","subtype":"init","session_id":"s1","model":"haiku"}`))
	u.Line([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"the credential is ` + lksr + ` and ` + session + `"}]}}`))
	u.Line([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","content":"env: ANTHROPIC_API_KEY=` + model + `"}]}}`))
	// the relay mints a git token mid-session
	mu.Lock()
	held = append(held, git)
	mu.Unlock()
	escaped := strings.Replace(git, "token", `\u0074oken`, 1)
	u.Line([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","content":"remote: ` + escaped + `"}]}}`))
	u.Line([]byte(`{"` + model + `":"as a key","pattern":"` + pattern + `"}`))
	u.Line([]byte(`not json at all: ` + git + " " + session))
	got := u.Close()

	sent := rc.all()
	for _, s := range []string{lksr, session, model, git, escaped, pattern} {
		if strings.Contains(sent, s) {
			t.Fatalf("the upload carries a secret %q:\n%s", s, sent)
		}
	}
	if strings.Count(sent, "[redacted]") < 7 || !strings.Contains(sent, `"model":"haiku"`) || !strings.Contains(sent, "not json at all") {
		t.Fatalf("the upload:\n%s", sent)
	}
	if got.Chunks != 1 || got.Failed != 0 || got.Bytes != int64(len(sent)) {
		t.Fatalf("upload %+v, %d bytes sent", got, len(sent))
	}
}

// TestChunkProtocol: whole lines go in chunks once ChunkBytes are held,
// each PUT with the product's headers, the lease, its seq and offset;
// the last carries final, the total and the digest of everything.
func TestChunkProtocol(t *testing.T) {
	rc, loc := start(t, 0)
	u := New(loc, "lease-7", nil, Options{ChunkBytes: 64, Every: time.Hour})
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"0123456789012345678901234567890123456789"}]}}`
	for range 5 {
		u.Line([]byte(line))
		time.Sleep(20 * time.Millisecond) // a full chunk goes before the next line
	}
	got := u.Close()
	rc.mu.Lock()
	chunks := append([]chunk(nil), rc.chunks...)
	rc.mu.Unlock()
	if len(chunks) < 2 || got.Chunks != int64(len(chunks)) || got.Failed != 0 {
		t.Fatalf("%d chunks, upload %+v", len(chunks), got)
	}
	var offset int64
	var all strings.Builder
	for i, c := range chunks {
		h := c.header
		if h.Get("X-Product") != "p1" || h.Get(HeaderLease) != "lease-7" || h.Get(HeaderSeq) != strconv.Itoa(i+1) ||
			h.Get(HeaderOffset) != strconv.FormatInt(offset, 10) || h.Get("Content-Type") != "application/x-ndjson" {
			t.Fatalf("chunk %d headers %v", i+1, h)
		}
		if c.body != "" && !strings.HasSuffix(c.body, "\n") {
			t.Fatalf("chunk %d is not whole lines: %q", i+1, c.body)
		}
		last := i == len(chunks)-1
		if (h.Get(HeaderFinal) == "true") != last {
			t.Fatalf("chunk %d final %q", i+1, h.Get(HeaderFinal))
		}
		offset += int64(len(c.body))
		all.WriteString(c.body)
	}
	sum := sha256.Sum256([]byte(all.String()))
	final := chunks[len(chunks)-1].header
	if final.Get(HeaderDigest) != "sha256:"+hex.EncodeToString(sum[:]) || final.Get(HeaderBytes) != strconv.Itoa(all.Len()) ||
		got.Digest != final.Get(HeaderDigest) || got.Bytes != int64(all.Len()) {
		t.Fatalf("final %v, upload %+v", final, got)
	}
	if again := u.Close(); again != got {
		t.Fatalf("a second close %+v", again)
	}
}

// TestUploadFailuresAreCounted: a location that refuses every chunk, or
// has expired, costs the session nothing: each chunk is tried, then
// counted failed; the bytes and digest are still the lease's.
func TestUploadFailuresAreCounted(t *testing.T) {
	_, loc := start(t, http.StatusInternalServerError)
	u := New(loc, "lease-1", nil, Options{Tries: 2, Backoff: time.Millisecond, Every: time.Hour})
	u.Line([]byte(`{"type":"result"}`))
	got := u.Close()
	if got.Chunks != 0 || got.Failed != 1 || got.Bytes == 0 || got.Digest == "" {
		t.Fatalf("a refusing location %+v", got)
	}

	rc, loc := start(t, 0)
	loc.ExpiresAt = time.Now().Add(-time.Minute)
	u = New(loc, "lease-1", nil, Options{Every: time.Hour})
	u.Line([]byte(`{"type":"result"}`))
	if got := u.Close(); got.Chunks != 0 || got.Failed != 1 || rc.all() != "" {
		t.Fatalf("an expired location %+v", got)
	}

	// a location too slow for the session: what is held past MaxHeld is
	// dropped and counted, and the session never waits
	rc, loc = start(t, 0)
	u = New(loc, "lease-1", nil, Options{MaxHeld: 100, ChunkBytes: 1 << 20, Every: time.Hour})
	for range 10 {
		u.Line([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"some words"}]}}`))
	}
	if got := u.Close(); got.Failed == 0 || got.Chunks != 1 {
		t.Fatalf("a slow location %+v", got)
	}
}
