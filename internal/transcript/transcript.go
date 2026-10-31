// Package transcript sends a lease's session stream, redacted, to the
// location the product named in the brief (projectmesh-io/api
// docs/adr/adr-001-amendment-learnings-and-transcripts.md,
// "Transcripts", and its chunk protocol). Mesh never sees a byte of it:
// the node sends straight to the product's location, and Mesh hears only
// {bytes, digest, chunks, failed} in the lease's usage report.
//
// Every line of the session's stream-json is redacted before it is
// kept: the lease's own secrets first (its session credential and
// tokens, the git tokens its relay minted, its model credential), then
// every secret pattern the node knows (tripline.Scrubber). A JSON line's
// string values are redacted decoded, so an escaped secret is caught
// too, and the line is written again.
//
// The chunk protocol: each chunk is an HTTP PUT to upload_url, with any
// headers the product's offer still names and these:
//
//	Authorization: DPoP <the lease's mesh-session token>
//	DPoP: <a fresh proof of this PUT>  htm PUT, htu upload_url without
//	                                   its query or fragment, ath of the token
//	Content-Type: application/x-ndjson
//	Mesh-Lease: <lease id>
//	Mesh-Transcript-Seq: <n>          1, 2, … per lease
//	Mesh-Transcript-Offset: <bytes>   where the chunk starts in the lease's transcript
//	Mesh-Transcript-Final: true       the last chunk alone, sent when the session ends
//	Mesh-Transcript-Bytes: <total>    the last chunk alone
//	Mesh-Transcript-Digest: sha256:<hex>   the last chunk alone: of the whole transcript
//
// The token and proof are the ones the lease's relay sends a tool call
// to the product on (mesh-specs gap 30): the lease's Latchkey session's
// token of the moment, so a token refreshed mid-lease carries the next
// chunk, and a proof by the session's own DPoP key. Mesh-Lease is the
// lease the token was minted for. A chunk answered 401 is sent once
// more at once, on a fresh proof carrying the DPoP-Nonce the product
// asked for if it asked one, and when that is refused too it is counted
// failed: the token is not this location's, and trying it again later
// would not make it so. Neither the token nor a proof is ever logged.
//
// A chunk's body is whole lines. A chunk goes once ChunkBytes are held
// or Every has passed with lines held; the last goes at Close, empty
// when nothing is held, so the product always hears the end. A chunk
// that is not answered 2xx is tried Tries times in all, then counted
// failed and dropped: its seq is not reused, so the product sees the
// gap. Nothing about the upload ever fails the job.
package transcript

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/tripline"
)

// The chunk protocol's headers.
const (
	HeaderLease  = "Mesh-Lease"
	HeaderSeq    = "Mesh-Transcript-Seq"
	HeaderOffset = "Mesh-Transcript-Offset"
	HeaderFinal  = "Mesh-Transcript-Final"
	HeaderBytes  = "Mesh-Transcript-Bytes"
	HeaderDigest = "Mesh-Transcript-Digest"
)

// Options tune an Uploader; the zero value is the node's.
type Options struct {
	// ChunkBytes sends a chunk once this much is held; 256 KiB when 0.
	ChunkBytes int
	// MaxHeld drops what is held, counted as one failed chunk, once a
	// slow location lets it grow past this; 8 MiB when 0.
	MaxHeld int
	// Every sends what is held this often; 15 s when 0.
	Every time.Duration
	// Tries is how often a chunk is tried; 3 when 0.
	Tries int
	// Backoff is the wait before the second try, doubled after; 1 s when 0.
	Backoff time.Duration
	// HTTP sends the chunks; a 30-second client when nil.
	HTTP *http.Client
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Logf hears a chunk that failed: never its URL, its body or its
	// credentials.
	Logf func(format string, args ...any)
	// Session is the lease's Latchkey session every chunk is sent on;
	// nil sends chunks with the offer's headers alone.
	Session Session
	// Named answers the lease's named values as they stand, each form
	// with the mark it reads as ([secret:<ref>]): redacted before any
	// other secret or pattern.
	Named func() map[string]string
}

// Session is a lease's Latchkey session as a chunk is sent on it: its
// mesh-session token of the moment, refreshed when near its end, and a
// DPoP proof by the key the token is bound to.
type Session interface {
	Token(ctx context.Context) (string, error)
	ProofWithNonce(method, rawURL, token, nonce string) (string, error)
}

// Secrets answers the lease's secrets as they stand: a lease mints more
// as it runs.
type Secrets func() []string

// Uploader is one lease's transcript upload.
type Uploader struct {
	loc     meshapi.Transcript
	lease   string
	secrets Secrets
	o       Options

	mu      sync.Mutex
	held    bytes.Buffer
	scrub   *tripline.Scrubber
	known   map[string]bool // every secret the lease has held: one refreshed away is still redacted
	all     []string
	named   int // how many named values the scrubber was made with
	sum     hash.Hash
	total   int64
	seq     int
	offset  int64 // the offset of the next chunk
	chunks  int64
	failed  int64
	closed  bool
	kick    chan struct{}
	stopped chan struct{}
	sending sync.Mutex
}

// New starts the lease's upload to loc.
func New(loc meshapi.Transcript, lease string, secrets Secrets, o Options) *Uploader {
	if o.ChunkBytes <= 0 {
		o.ChunkBytes = 256 << 10
	}
	if o.MaxHeld <= 0 {
		o.MaxHeld = 8 << 20
	}
	if o.Every <= 0 {
		o.Every = 15 * time.Second
	}
	if o.Tries <= 0 {
		o.Tries = 3
	}
	if o.Backoff <= 0 {
		o.Backoff = time.Second
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if secrets == nil {
		secrets = func() []string { return nil }
	}
	u := &Uploader{loc: loc, lease: lease, secrets: secrets, o: o, sum: sha256.New(), kick: make(chan struct{}, 1), stopped: make(chan struct{})}
	go u.loop()
	return u
}

// Line takes one line of the session's stream: redacted, kept, and sent
// with the next chunk. It never blocks on the network.
func (u *Uploader) Line(line []byte) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return
	}
	out := append(u.redact(line), '\n')
	u.sum.Write(out)
	u.total += int64(len(out))
	u.held.Write(out)
	if u.held.Len() > u.o.MaxHeld {
		// the location is too slow for the session: what is held goes,
		// counted, and the next chunk starts after it
		u.offset += int64(u.held.Len())
		u.held.Reset()
		u.seq++
		u.failed++
		return
	}
	if u.held.Len() >= u.o.ChunkBytes {
		select {
		case u.kick <- struct{}{}:
		default:
		}
	}
}

// redact is line with the lease's secrets and every secret pattern
// redacted; held under mu.
func (u *Uploader) redact(line []byte) []byte {
	grew := u.scrub == nil
	for _, s := range u.secrets() {
		if s != "" && !u.known[s] {
			if u.known == nil {
				u.known = map[string]bool{}
			}
			u.known[s] = true
			u.all = append(u.all, s)
			grew = true
		}
	}
	var named map[string]string
	if u.o.Named != nil {
		named = u.o.Named()
		if len(named) != u.named {
			u.named, grew = len(named), true
		}
	}
	if grew {
		u.scrub = tripline.NewScrubber(tripline.Options{Secrets: u.all, Named: named})
	}
	return Redact(u.scrub, line)
}

// Redact is line redacted by s: a JSON line's string values decoded and
// redacted, then the line as written again redacted whole; any other
// line redacted as text.
func Redact(s *tripline.Scrubber, line []byte) []byte {
	var v any
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if dec.Decode(&v) != nil || dec.More() {
		return []byte(s.Text(string(line)))
	}
	v = walk(s, v)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return []byte(s.Text(string(line)))
	}
	return []byte(s.Text(string(bytes.TrimRight(buf.Bytes(), "\n"))))
}

func walk(s *tripline.Scrubber, v any) any {
	switch t := v.(type) {
	case string:
		return s.Text(t)
	case []any:
		for i := range t {
			t[i] = walk(s, t[i])
		}
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[s.Text(k)] = walk(s, e)
		}
		return out
	}
	return v
}

// loop sends a chunk when one is full or Every passes, until Close.
func (u *Uploader) loop() {
	t := time.NewTicker(u.o.Every)
	defer t.Stop()
	for {
		select {
		case <-u.stopped:
			return
		case <-u.kick:
		case <-t.C:
		}
		u.send(false)
	}
}

// send sends what is held as the next chunk; the final one even when
// nothing is held.
func (u *Uploader) send(final bool) {
	u.sending.Lock()
	defer u.sending.Unlock()
	u.mu.Lock()
	if u.held.Len() == 0 && !final {
		u.mu.Unlock()
		return
	}
	body := append([]byte(nil), u.held.Bytes()...)
	u.held.Reset()
	u.seq++
	seq, offset := u.seq, u.offset
	u.offset += int64(len(body))
	total, digest := u.total, "sha256:"+hex.EncodeToString(u.sum.Sum(nil))
	u.mu.Unlock()

	ok := u.put(body, seq, offset, final, total, digest)
	u.mu.Lock()
	if ok {
		u.chunks++
	} else {
		u.failed++
	}
	u.mu.Unlock()
}

// put sends one chunk, trying again on failure, and answers whether it
// landed. A location past its expiry is not tried.
func (u *Uploader) put(body []byte, seq int, offset int64, final bool, total int64, digest string) bool {
	if !u.loc.ExpiresAt.IsZero() && !u.o.Now().Before(u.loc.ExpiresAt) {
		u.logf("transcript: lease %s: chunk %d not sent: the location expired", u.lease, seq)
		return false
	}
	wait := u.o.Backoff
	for try := 1; ; try++ {
		status, err := u.try(body, seq, offset, final, total, digest)
		if err == nil && status/100 == 2 {
			return true
		}
		why := fmt.Sprintf("answered %d", status)
		if errors.Is(err, errNoToken) {
			why = "could not be sent: the lease's session token could not be had"
		} else if err != nil {
			why = "unreachable"
		}
		if status == http.StatusUnauthorized {
			// refused on a fresh proof twice: the token is not this
			// location's, and will not be on a later try
			u.logf("transcript: lease %s: chunk %d dropped: the location refused the lease's session token", u.lease, seq)
			return false
		}
		if try >= u.o.Tries {
			u.logf("transcript: lease %s: chunk %d dropped after %d tries: the location %s", u.lease, seq, try, why)
			return false
		}
		time.Sleep(wait)
		wait *= 2
	}
}

// errNoToken is a chunk not sent because the session answered no token.
var errNoToken = errors.New("transcript: no session token")

// try sends the chunk once on the session's token of the moment, and
// once more on a fresh proof when the location answers 401 — with the
// DPoP-Nonce it asked for, if it asked one.
func (u *Uploader) try(body []byte, seq int, offset int64, final bool, total int64, digest string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	token := ""
	if u.o.Session != nil {
		var err error
		if token, err = u.o.Session.Token(ctx); err != nil || token == "" {
			return 0, errNoToken
		}
	}
	status, h, err := u.tryOnce(ctx, body, seq, offset, final, total, digest, token, "")
	if err != nil || status != http.StatusUnauthorized || u.o.Session == nil {
		return status, err
	}
	status, _, err = u.tryOnce(ctx, body, seq, offset, final, total, digest, token, h.Get("DPoP-Nonce"))
	return status, err
}

func (u *Uploader) tryOnce(ctx context.Context, body []byte, seq int, offset int64, final bool, total int64, digest, token, nonce string) (int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.loc.UploadURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range u.loc.Headers {
		req.Header.Set(k, v)
	}
	if token != "" {
		proof, err := u.o.Session.ProofWithNonce(http.MethodPut, u.loc.UploadURL, token, nonce)
		if err != nil {
			return 0, nil, errNoToken
		}
		req.Header.Set("Authorization", "DPoP "+token)
		req.Header.Set("DPoP", proof)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set(HeaderLease, u.lease)
	req.Header.Set(HeaderSeq, strconv.Itoa(seq))
	req.Header.Set(HeaderOffset, strconv.FormatInt(offset, 10))
	if final {
		req.Header.Set(HeaderFinal, "true")
		req.Header.Set(HeaderBytes, strconv.FormatInt(total, 10))
		req.Header.Set(HeaderDigest, digest)
	}
	resp, err := u.o.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, resp.Header, nil
}

// Close sends what is held as the final chunk and answers what the
// lease sent: its bytes, digest, the chunks that landed and those that
// did not. Closing twice answers the same.
func (u *Uploader) Close() meshapi.TranscriptUpload {
	if u == nil {
		return meshapi.TranscriptUpload{}
	}
	u.mu.Lock()
	first := !u.closed
	u.closed = true
	u.mu.Unlock()
	if first {
		close(u.stopped)
		u.send(true)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return meshapi.TranscriptUpload{Bytes: u.total, Digest: "sha256:" + hex.EncodeToString(u.sum.Sum(nil)), Chunks: u.chunks, Failed: u.failed}
}

func (u *Uploader) logf(format string, args ...any) {
	if u.o.Logf != nil {
		u.o.Logf(format, args...)
	}
}
