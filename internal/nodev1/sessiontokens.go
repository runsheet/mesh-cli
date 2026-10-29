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

// MintSource mints the lease's git token for repo at Latchkey: POST
// /v1/credentials/for-job/source {repository, access} on the session
// token. Latchkey checks the lease is live with Mesh.
func (s *sessionTokens) MintSource(ctx context.Context, repo, access string) (gitrelay.Token, error) {
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := s.onToken(ctx, http.MethodPost, sourcePath, map[string]string{"repository": repo, "access": access}, &out); err != nil {
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
// token>` and a proof of the request.
func (s *sessionTokens) onToken(ctx context.Context, method, path string, body, out any) error {
	tok, err := s.Token(ctx)
	if err != nil {
		return err
	}
	iss, err := s.issuer()
	if err != nil {
		return err
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
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
