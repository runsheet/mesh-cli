package nodev1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// The session is the job's one key to Latchkey (Mesh v1 spec, "The git
// relay and the Latchkey token relay"). For each lease the node makes a
// P-256 key that lives only in this process, asks Mesh to open the
// lease's Latchkey session bound to its thumbprint (POST
// /v1/leases/{id}/session → lksr_…, once), and trades the request
// credential at Latchkey for DPoP-bound mesh-session tokens. Every
// job-scoped credential is asked for on that token: the git token
// (POST /v1/credentials/for-job/source) here. The tokens, the request
// credential and the key are held by sessionTokens alone — the relay and
// the sessionproxy reach them through it — and are never in the
// session's environment, on disk or in a checkout's git config.

// MeshSessionAudience is the audience a lease's session tokens carry.
const MeshSessionAudience = "mesh-session"

// SigstoreAudience is the audience of the lease's signing token: the ID
// token Fulcio takes (Latchkey ADR 017 decision 3 and its amendment of
// 2026-10-30), its client id `sigstore`. Latchkey never mints it
// DPoP-bound, since Fulcio cannot check a proof. A mesh-session token is
// refused by Fulcio ("expected audience sigstore").
const SigstoreAudience = "sigstore"

// sourcePath is Latchkey's for-job source-token route.
const sourcePath = "/v1/credentials/for-job/source"

// tokenRefreshBefore is how long before expiry a session token is
// replaced.
const tokenRefreshBefore = time.Minute

// sessionTokens is one lease's Latchkey session: its key, its request
// credential, and the session token of the moment.
type sessionTokens struct {
	cred *meshapi.SessionCredential
	key  *dpop.Key
	http *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
	// signing is the lease's sigstore-audience ID token of the moment
	// (SigningToken) and when it ends
	signing        string
	signingExpires time.Time
}

// openSession makes the lease's key and has Mesh open its session.
func openSession(ctx context.Context, api *meshapi.Client, leaseID string, hc *http.Client) (*sessionTokens, error) {
	key, err := dpop.NewKey()
	if err != nil {
		return nil, err
	}
	cred, err := api.Session(ctx, leaseID, key.Thumbprint())
	if err != nil {
		return nil, fmt.Errorf("opening the lease's session: %w", err)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &sessionTokens{cred: cred, key: key, http: hc}, nil
}

// String names the session alone.
func (s *sessionTokens) String() string { return s.cred.String() }

// Key is the DPoP key the session's tokens are bound to.
func (s *sessionTokens) Key() *dpop.Key { return s.key }

// issuer is Latchkey's origin: the request URL's scheme and host.
func (s *sessionTokens) issuer() (string, error) {
	u, err := url.Parse(s.cred.RequestURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("the session's request url %q is not absolute", s.cred.RequestURL)
	}
	return u.Scheme + "://" + u.Host, nil
}

// Token answers a live mesh-session token, minting one at the request
// URL on the request credential with a DPoP proof when the last is near
// its end.
func (s *sessionTokens) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Add(tokenRefreshBefore).Before(s.expires) {
		return s.token, nil
	}
	u, err := url.Parse(s.cred.RequestURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("audience", MeshSessionAudience)
	u.RawQuery = q.Encode()
	raw, status, err := s.send(ctx, http.MethodGet, u.String(), "Bearer "+s.cred.RequestToken, "", nil)
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		return "", fmt.Errorf("%s: the session token was refused: %d %s", s, status, latchkeyMessage(raw))
	}
	var out struct {
		Value     string `json:"value"`
		ExpiresIn int    `json:"expires_in"`
		TokenType string `json:"token_type"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("%s: session token: %w", s, err)
	}
	if out.Value == "" || !strings.EqualFold(out.TokenType, "DPoP") {
		return "", fmt.Errorf("%s: Latchkey answered no DPoP-bound token", s)
	}
	s.token, s.expires = out.Value, time.Now().Add(time.Duration(out.ExpiresIn)*time.Second)
	return s.token, nil
}

// SigningToken answers a live ID token of the lease's session for
// Fulcio: minted at the request URL for audience `sigstore` on the
// request credential, unbound (Latchkey ADR 017 decision 3), and kept
// until a minute before it ends. Its subject is the session's, so the
// certificate Fulcio issues names the job and lease. It is held here
// alone, like the session token, and never reaches the pod.
func (s *sessionTokens) SigningToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.signing != "" && time.Now().Add(tokenRefreshBefore).Before(s.signingExpires) {
		return s.signing, nil
	}
	u, err := url.Parse(s.cred.RequestURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("audience", SigstoreAudience)
	u.RawQuery = q.Encode()
	// the proof send adds is not read for an unbound audience
	raw, status, err := s.send(ctx, http.MethodGet, u.String(), "Bearer "+s.cred.RequestToken, "", nil)
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		return "", fmt.Errorf("%s: the signing token (audience %s) was refused: %d %s", s, SigstoreAudience, status, latchkeyMessage(raw))
	}
	var out struct {
		Value     string `json:"value"`
		ExpiresIn int    `json:"expires_in"`
		TokenType string `json:"token_type"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("%s: signing token: %w", s, err)
	}
	if out.Value == "" {
		return "", fmt.Errorf("%s: Latchkey answered no signing token", s)
	}
	if strings.EqualFold(out.TokenType, "DPoP") {
		return "", fmt.Errorf("%s: Latchkey answered a DPoP-bound signing token, which Fulcio cannot take", s)
	}
	s.signing, s.signingExpires = out.Value, time.Now().Add(time.Duration(out.ExpiresIn)*time.Second)
	return s.signing, nil
}

// SourceRequest is Latchkey's for-job source mint (L3): the repository
// and access, and the vendor and its base when the brief names them —
// GitHub at its own origin when it names neither, which Latchkey takes
// as before vendors were named (Mesh v1 2.9).
type SourceRequest struct {
	Vendor     string `json:"vendor,omitempty"`
	BaseURL    string `json:"base_url,omitempty"`
	Repository string `json:"repository"`
	Access     string `json:"access"`
}

// MintSource mints the lease's git token at Latchkey: POST
// /v1/credentials/for-job/source {vendor, base_url, repository, access}
// on the session token. Latchkey checks the lease is live with Mesh and
// mints through the tenant's connection for that vendor.
func (s *sessionTokens) MintSource(ctx context.Context, req SourceRequest) (gitrelay.Token, error) {
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := s.onToken(ctx, http.MethodPost, sourcePath, req, &out); err != nil {
		return gitrelay.Token{}, err
	}
	if out.Token == "" {
		return gitrelay.Token{}, errors.New("Latchkey answered no source token")
	}
	return gitrelay.Token{Value: out.Token, ExpiresAt: out.ExpiresAt}, nil
}

// RevokeSource revokes each minted git token: DELETE
// /v1/credentials/for-job/source {token}. Every token is tried; the
// first error is answered.
func (s *sessionTokens) RevokeSource(ctx context.Context, minted []gitrelay.Token) error {
	var first error
	for _, t := range minted {
		if err := s.onToken(ctx, http.MethodDelete, sourcePath, map[string]string{"token": t.Value}, nil); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// onToken calls Latchkey at path with `Authorization: DPoP <session
// token>` and a proof of the request; a nil body sends none.
func (s *sessionTokens) onToken(ctx context.Context, method, path string, body, out any) error {
	tok, err := s.Token(ctx)
	if err != nil {
		return err
	}
	iss, err := s.issuer()
	if err != nil {
		return err
	}
	var b []byte
	if body != nil {
		if b, err = json.Marshal(body); err != nil {
			return err
		}
	}
	raw, status, err := s.send(ctx, method, iss+path, "DPoP "+tok, tok, b)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("Latchkey %s %s: %d %s", method, path, status, latchkeyMessage(raw))
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// send is one request with a DPoP proof of it (ath over proofToken when
// set), asked once more with Latchkey's nonce when it asks for one.
// Nothing it answers or errs names a credential.
func (s *sessionTokens) send(ctx context.Context, method, rawURL, authorization, proofToken string, body []byte) ([]byte, int, error) {
	try := func(nonce string) ([]byte, int, http.Header, error) {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
		if err != nil {
			return nil, 0, nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", authorization)
		proof, err := s.key.ProofWithNonce(method, rawURL, proofToken, nonce)
		if err != nil {
			return nil, 0, nil, err
		}
		req.Header.Set("DPoP", proof)
		resp, err := s.http.Do(req)
		if err != nil {
			return nil, 0, nil, errors.New(scrub(err.Error(), s.cred.RequestToken, proofToken))
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return raw, resp.StatusCode, resp.Header, err
	}
	raw, status, h, err := try("")
	if err != nil {
		return nil, 0, err
	}
	if nonce := h.Get("DPoP-Nonce"); nonce != "" && (status == http.StatusBadRequest || status == http.StatusUnauthorized) {
		raw, status, _, err = try(nonce)
	}
	return raw, status, err
}

// latchkeyMessage is an OAuth-style refusal's description, else a short
// body.
func latchkeyMessage(raw []byte) string {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(raw, &e) == nil && (e.Error != "" || e.Description != "") {
		return strings.TrimSpace(e.Error + ": " + e.Description)
	}
	if s := strings.TrimSpace(string(raw)); len(s) < 300 {
		return s
	}
	return ""
}

func scrub(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

// current is a live session token and when it ends: Token's, for the
// lease's relay sidecar, which holds it until near that end.
func (s *sessionTokens) current(ctx context.Context) (string, time.Time, error) {
	tok, err := s.Token(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return tok, s.expires, nil
}
