// Package results sends a node tool's full, raw output straight to the
// location the product named in the brief — never to Mesh's own blob
// storage. The caller keeps only the digest and byte count Upload
// answers: a signed record's output subject names the digest alone
// (projectmesh-io/api docs/adr/adr-001-amendment-learnings-and-
// transcripts.md's results location, the transcript's one-shot
// sibling).
//
// One HTTP PUT of the whole body, with any headers the product's offer
// still names and these:
//
//	Authorization: DPoP <the lease's mesh-session token>
//	DPoP: <a fresh proof of this PUT>  htm PUT, htu upload_url without
//	                                   its query or fragment, ath of the token
//
// A PUT answered other than 2xx is tried again, Tries times in all,
// with a doubling backoff between tries. A 401 is retried once at
// once on a fresh proof — carrying the DPoP-Nonce the product asked
// for, if it asked one — and a second 401 is not retried further: the
// token is not this location's, and trying it again would not make it
// so. A location past its expiry is not tried at all.
package results

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// Session is a lease's Latchkey session as the PUT is sent on it: its
// mesh-session token of the moment, and a DPoP proof by the key the
// token is bound to.
type Session interface {
	Token(ctx context.Context) (string, error)
	ProofWithNonce(method, rawURL, token, nonce string) (string, error)
}

// Options tune Upload; the zero value is the node's.
type Options struct {
	// Tries is how often the PUT is tried; 3 when 0.
	Tries int
	// Backoff is the wait before the second try, doubled after; 1 s when 0.
	Backoff time.Duration
	// HTTP sends the PUT; a 30-second client when nil.
	HTTP *http.Client
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Logf hears a try that failed: never its URL, its body or its
	// credentials.
	Logf func(format string, args ...any)
	// Session is the lease's Latchkey session the PUT is sent on; nil
	// sends it with the location's headers alone.
	Session Session
}

// Result is what Upload sent: the body's digest and byte count.
type Result struct {
	Digest string
	Bytes  int64
}

// errNoToken is the PUT not sent because the session answered no token.
var errNoToken = errors.New("results: no session token")

// ErrExpired is Upload refused: the location's ExpiresAt has passed.
var ErrExpired = errors.New("results: the location expired")

// ErrRefused is Upload refused: the location answered 401 twice, so the
// token is not this location's.
var ErrRefused = errors.New("results: the location refused the lease's session token")

// Upload sends body whole to loc, trying again on failure, and answers
// its digest and byte count once a try lands 2xx.
func Upload(ctx context.Context, loc meshapi.ResultsLocation, body []byte, o Options) (Result, error) {
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
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	result := Result{Digest: digest, Bytes: int64(len(body))}
	if !loc.ExpiresAt.IsZero() && !o.Now().Before(loc.ExpiresAt) {
		o.logf("results: upload not sent: the location expired")
		return Result{}, ErrExpired
	}
	wait := o.Backoff
	for try := 1; ; try++ {
		status, err := try1(ctx, loc, body, o)
		if err == nil && status/100 == 2 {
			return result, nil
		}
		why := fmt.Sprintf("answered %d", status)
		if errors.Is(err, errNoToken) {
			why = "could not be sent: the lease's session token could not be had"
		} else if err != nil {
			why = "unreachable"
		}
		if status == http.StatusUnauthorized {
			o.logf("results: upload dropped: the location refused the lease's session token")
			return Result{}, ErrRefused
		}
		if try >= o.Tries {
			o.logf("results: upload dropped after %d tries: the location %s", try, why)
			return Result{}, fmt.Errorf("results: upload dropped after %d tries: the location %s", try, why)
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// try1 sends the PUT once on the session's token of the moment, and
// once more on a fresh proof when the location answers 401 — with the
// DPoP-Nonce it asked for, if it asked one.
func try1(ctx context.Context, loc meshapi.ResultsLocation, body []byte, o Options) (int, error) {
	token := ""
	if o.Session != nil {
		var err error
		if token, err = o.Session.Token(ctx); err != nil || token == "" {
			return 0, errNoToken
		}
	}
	status, h, err := putOnce(ctx, loc, body, o, token, "")
	if err != nil || status != http.StatusUnauthorized || o.Session == nil {
		return status, err
	}
	status, _, err = putOnce(ctx, loc, body, o, token, h.Get("DPoP-Nonce"))
	return status, err
}

func putOnce(ctx context.Context, loc meshapi.ResultsLocation, body []byte, o Options, token, nonce string) (int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, loc.UploadURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range loc.Headers {
		req.Header.Set(k, v)
	}
	if token != "" {
		proof, err := o.Session.ProofWithNonce(http.MethodPut, loc.UploadURL, token, nonce)
		if err != nil {
			return 0, nil, errNoToken
		}
		req.Header.Set("Authorization", "DPoP "+token)
		req.Header.Set("DPoP", proof)
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, resp.Header, nil
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}
