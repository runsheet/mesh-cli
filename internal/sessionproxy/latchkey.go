package sessionproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Every vendor API write goes through the relay (Mesh v1 spec, "Many git
// vendors, and every write through the relay", wave 2.7): a session
// opens a pull request, comments, merges or sets a status by calling
// /latchkey/<vendor>/<path> on its lease's socket. The proxy forwards the
// call to Latchkey's API proxy for that vendor, <Latchkey>/<vendor>/<path>,
// on the lease's DPoP-bound mesh-session token with a fresh proof of the
// request — Latchkey L4 holds it to the job's access.api rules, adds the
// vendor credential the session never sees, and records it with the job
// and the lease. The node checks the same rules first, so a call no rule
// allows never leaves it, and every call — relayed or refused — is heard
// by Audit, which nodev1 posts to Mesh as the lease's AccessRecords.

// LatchkeyPrefix is the path a session reaches a vendor's API under:
// /latchkey/<vendor>/<path>.
const LatchkeyPrefix = "/latchkey/"

// RelayErrorHeader names the node's own refusal of a vendor API call, as
// Latchkey's X-Latchkey-Error names Latchkey's: a session can tell "the
// relay said no" from "the vendor said no".
const RelayErrorHeader = "X-Mesh-Relay-Error"

const (
	// apiBodyMax caps a buffered request body: a vendor's JSON payloads
	// are small (Latchkey's own cap for buffered calls).
	apiBodyMax = 4 << 20
	// apiStreamMax caps a body sent on with its declared length, never
	// held: a blob's content (Latchkey's streaming cap).
	apiStreamMax = 50 << 20
	// apiResponseMax caps what is relayed back.
	apiResponseMax = 16 << 20
)

// apiMethods are the methods a vendor's REST API takes.
var apiMethods = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// apiPath is a vendor API path: segments of the characters vendors route
// on, so traversal and control characters are refused by construction.
var apiPath = regexp.MustCompile(`^[A-Za-z0-9._~%@:+!,;=-]+(/[A-Za-z0-9._~%@:+!,;=-]+)*$`)

// apiVendor is a vendor's name as a brief and Latchkey's routes name it.
var apiVendor = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// apiForwarded are the only client headers sent on: what a vendor's REST
// API reads. Authorization, cookies and anything else never leave.
var apiForwarded = []string{"Content-Type", "Accept", "If-None-Match", "If-Match", "If-Modified-Since", "User-Agent", "X-GitHub-Api-Version"}

// apiReturned are the only Latchkey response headers sent back.
var apiReturned = []string{
	"Content-Type", "ETag", "Last-Modified", "Location", "Retry-After", "Link",
	"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "X-RateLimit-Used", "X-RateLimit-Resource",
	"X-GitHub-Request-Id", "X-Latchkey-Error",
}

// Latchkey is Latchkey's API proxy for one lease.
type Latchkey struct {
	// URL is Latchkey's origin: /latchkey/<vendor>/<path> goes to
	// URL/<vendor>/<path>.
	URL string
	// Token answers the lease's live mesh-session token, held in memory
	// for the one call.
	Token func(ctx context.Context) (string, error)
	// Key signs each call's DPoP proof; a NonceSigner answers Latchkey's
	// nonce when it asks for one.
	Key Signer
	// Allow is the job's access.api check, made before anything leaves
	// the node: a call it refuses is answered 403 and audited. Nil
	// refuses every call, since a job whose brief names no API rule may
	// make none.
	Allow func(vendor, method, path string) bool
	// Audit hears every call, relayed or refused; it must not block.
	Audit func(APICall)
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// NonceSigner is a Signer that carries a server's DPoP nonce
// (internal/dpop.Key).
type NonceSigner interface {
	ProofWithNonce(method, rawURL, token, nonce string) (string, error)
}

// APICall is one vendor API call a session made through the relay.
type APICall struct {
	Vendor string
	Method string
	// Path is the API path, without its query: never a body, a query or
	// a credential.
	Path string
	// Status is what Latchkey (or the vendor through it) answered; 0
	// when the node refused or could not reach Latchkey.
	Status int
	// Outcome is ok, refused or failed; Reason says why when not ok.
	Outcome string
	Reason  string
	Bytes   int64
	At      time.Time
}

// serveLatchkey relays one /latchkey/<vendor>/<path> call.
func (p *Proxy) serveLatchkey(w http.ResponseWriter, req *http.Request) {
	l := p.Latchkey
	// the path as sent: an escaped segment (a GitLab project path) goes on
	// escaped, never decoded into more segments
	rest := strings.TrimPrefix(req.URL.EscapedPath(), LatchkeyPrefix)
	vendor, path, _ := strings.Cut(rest, "/")
	call := APICall{Vendor: vendor, Method: req.Method, Path: path, At: l.now()}
	refuse := func(status int, code, why string) {
		call.Status, call.Outcome, call.Reason = 0, "refused", why
		l.audit(call)
		p.logf("sessionproxy: latchkey: refused %s %s/%s: %s", req.Method, vendor, path, why)
		w.Header().Set(RelayErrorHeader, code)
		writeJSON(w, status, map[string]string{"error": code, "message": "relay: " + why})
	}
	switch {
	case !apiVendor.MatchString(vendor):
		refuse(http.StatusNotFound, "bad_vendor", fmt.Sprintf("%q is not a vendor", vendor))
		return
	case !apiMethods[req.Method]:
		refuse(http.StatusMethodNotAllowed, "bad_method", req.Method+" is not a method a vendor API takes")
		return
	case !apiPath.MatchString(path) || dotSegment(path):
		refuse(http.StatusBadRequest, "bad_path", "the path is not a vendor API path")
		return
	case l.Allow == nil || !l.Allow(vendor, req.Method, path):
		refuse(http.StatusForbidden, "policy_denied", "the job's access.api allows no "+req.Method+" "+path+" on "+vendor)
		return
	}

	// A body within apiBodyMax is held, so the call can be asked again
	// with Latchkey's DPoP nonce; a larger one goes on as it arrives with
	// the length it declared, which it must, up to apiStreamMax.
	var held []byte
	var stream io.Reader
	length := int64(0)
	switch {
	case req.Body == nil || req.Method == http.MethodGet || req.Method == http.MethodHead:
	case req.ContentLength > apiBodyMax:
		if req.ContentLength > apiStreamMax {
			refuse(http.StatusRequestEntityTooLarge, "body_too_large", "the request body is over the relay's limit")
			return
		}
		stream, length = http.MaxBytesReader(w, req.Body, req.ContentLength), req.ContentLength
	default:
		b, err := io.ReadAll(io.LimitReader(req.Body, apiBodyMax+1))
		if err != nil {
			refuse(http.StatusBadRequest, "bad_body", "reading the request body: "+err.Error())
			return
		}
		if len(b) > apiBodyMax {
			if req.ContentLength < 0 {
				refuse(http.StatusLengthRequired, "length_required", "a body over 4 MiB needs a Content-Length")
				return
			}
			refuse(http.StatusRequestEntityTooLarge, "body_too_large", "the request body is over the relay's limit")
			return
		}
		held, length = b, int64(len(b))
	}

	target := strings.TrimRight(l.URL, "/") + "/" + vendor + "/" + path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	send := func(nonce string) (*http.Response, error) {
		var body io.Reader
		switch {
		case held != nil:
			body = bytes.NewReader(held)
		case stream != nil:
			body = stream
		}
		out, err := http.NewRequestWithContext(req.Context(), req.Method, target, body)
		if err != nil {
			return nil, err
		}
		if body != nil {
			out.ContentLength = length
		}
		for _, k := range apiForwarded {
			if v := req.Header.Values(k); len(v) > 0 {
				out.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
			}
		}
		secrets, err := l.authorize(req.Context(), out, nonce)
		p.remember(secrets)
		if err != nil {
			return nil, err
		}
		client := p.HTTP
		if client == nil {
			client = &http.Client{}
		}
		return client.Do(out)
	}
	resp, err := send("")
	if err == nil && stream == nil {
		if nonce := resp.Header.Get("DPoP-Nonce"); nonce != "" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusBadRequest) {
			resp.Body.Close()
			resp, err = send(nonce)
		}
	}
	if err != nil {
		msg := p.mask(err.Error())
		call.Outcome, call.Reason = "failed", msg
		l.audit(call)
		p.logf("sessionproxy: latchkey: %s %s/%s: %s", req.Method, vendor, path, msg)
		w.Header().Set(RelayErrorHeader, "latchkey_unreachable")
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "latchkey_unreachable", "message": "relay: " + msg})
		return
	}
	defer resp.Body.Close()
	for _, k := range apiReturned {
		if v := resp.Header.Values(k); len(v) > 0 {
			w.Header()[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	// Latchkey's Link points pagination back at itself; the session
	// follows it through this route instead
	if link := resp.Header.Get("Link"); link != "" {
		base := "http://" + hostOf(req) + strings.TrimSuffix(LatchkeyPrefix, "/")
		w.Header().Set("Link", strings.ReplaceAll(link, strings.TrimRight(l.URL, "/"), base))
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, apiResponseMax))
	masked := p.mask(string(raw))
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	if req.Method != http.MethodHead {
		_, _ = io.WriteString(w, masked)
	}
	call.Status, call.Bytes, call.Outcome = resp.StatusCode, int64(len(raw)), "ok"
	switch {
	case rerr != nil:
		call.Outcome, call.Reason = "failed", "reading the answer: "+p.mask(rerr.Error())
	case resp.Header.Get("X-Latchkey-Error") != "":
		call.Outcome, call.Reason = "refused", "latchkey: "+resp.Header.Get("X-Latchkey-Error")
	case resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotModified:
		call.Outcome, call.Reason = "failed", fmt.Sprintf("%s answered %d", vendor, resp.StatusCode)
	}
	l.audit(call)
}

// authorize puts the session token and a proof of out on it: the proof
// signs out's method and URL (its query dropped, as DPoP's htu is) and
// the token's hash, with Latchkey's nonce when it gave one.
func (l *Latchkey) authorize(ctx context.Context, out *http.Request, nonce string) ([]string, error) {
	if l.Token == nil || l.Key == nil {
		return nil, errors.New("the lease has no Latchkey session")
	}
	tok, err := l.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("session token: %w", err)
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return nil, errors.New("the session token is empty")
	}
	var proof string
	if ns, ok := l.Key.(NonceSigner); ok && nonce != "" {
		proof, err = ns.ProofWithNonce(out.Method, out.URL.String(), tok, nonce)
	} else {
		proof, err = l.Key.Proof(out.Method, out.URL.String(), tok)
	}
	if err != nil {
		return []string{tok}, fmt.Errorf("dpop: %w", err)
	}
	out.Header.Set("Authorization", "DPoP "+tok)
	out.Header.Set("DPoP", proof)
	return []string{tok, proof}, nil
}

func (l *Latchkey) audit(c APICall) {
	if l.Audit != nil {
		l.Audit(c)
	}
}

func (l *Latchkey) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// dotSegment is whether a path segment is, or decodes to, . or ..
func dotSegment(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if d, err := url.PathUnescape(seg); err != nil || d == "." || d == ".." {
			return true
		}
	}
	return false
}

// hostOf is the host the session called the socket as, "relay" when it
// named none.
func hostOf(req *http.Request) string {
	if req.Host != "" {
		return req.Host
	}
	return "relay"
}
