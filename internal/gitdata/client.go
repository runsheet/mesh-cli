// Package gitdata is a small GitHub REST client over the Git Data and
// commit-comparison reads a node's validation needs to read a pushed
// commit without fetching it (goal
// a-node-validates-from-a-served-diff-with-a-shared-cache): Commit,
// Compare, Tree and Blob. The token travels per call, never held on the
// Client, so a session can mint or renew it item by item
// (node.ItemCred's ForFetch) or batch by batch, the same way a git
// fetch goes out on whatever ctx carries (internal/node's
// OnItemToken/withGitToken).
package gitdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DefaultBaseURL is the GitHub REST API's own.
const DefaultBaseURL = "https://api.github.com"

// TokenSource answers the token for one call, asked fresh each time so a
// renewed token (ItemCred.ForFetch) reaches the next request without a
// new Client.
type TokenSource func(ctx context.Context) (string, error)

// Client is a GitHub REST client over Git Data and commit comparisons.
// The zero value talks to the real API on http.DefaultClient with no
// token; set BaseURL to point at a test server, HTTP for a tuned
// client, and either Token or a token on ctx (WithToken) for every call
// that needs one. A Client is safe for concurrent use.
type Client struct {
	// BaseURL is the API root, "https://api.github.com" when empty.
	BaseURL string
	// HTTP is the client every request runs on; http.DefaultClient when
	// nil.
	HTTP *http.Client
	// Token, when set, is asked for the token on every call in place of
	// one on ctx.
	Token TokenSource
}

type tokenCtxKey struct{}

// WithToken is ctx carrying tok for every Client call made with it, read
// when the Client has no Token of its own.
func WithToken(ctx context.Context, tok string) context.Context {
	return context.WithValue(ctx, tokenCtxKey{}, tok)
}

func tokenFromContext(ctx context.Context) string {
	t, _ := ctx.Value(tokenCtxKey{}).(string)
	return t
}

func (c *Client) token(ctx context.Context) (string, error) {
	if c.Token != nil {
		return c.Token(ctx)
	}
	return tokenFromContext(ctx), nil
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// StatusError is a GitHub API call that answered past 2xx: the status
// code, the request and up to 4KB of the body, never the token.
type StatusError struct {
	StatusCode int
	Method     string
	URL        string
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("github api: %s %s: %d: %s", e.Method, e.URL, e.StatusCode, e.Body)
}

// StatusErrorOf is err's *StatusError, or nil.
func StatusErrorOf(err error) *StatusError {
	var se *StatusError
	if errors.As(err, &se) {
		return se
	}
	return nil
}

// IsUnauthorized is err a 401: the token is missing, malformed or spent.
func IsUnauthorized(err error) bool { return statusIs(err, http.StatusUnauthorized) }

// IsForbidden is err a 403: the token cannot do this on the repo (an
// installation token outside its one repo, a rate limit).
func IsForbidden(err error) bool { return statusIs(err, http.StatusForbidden) }

// IsNotFound is err a 404: the repo, ref, sha or path does not exist, or
// the token cannot see it (GitHub answers a private repo's absence the
// same way as its non-existence).
func IsNotFound(err error) bool { return statusIs(err, http.StatusNotFound) }

func statusIs(err error, code int) bool {
	se := StatusErrorOf(err)
	return se != nil && se.StatusCode == code
}

// get runs one GET against the API, decoding a 2xx JSON body into out;
// a non-2xx status is a *StatusError, never decoded.
func (c *Client) get(ctx context.Context, url string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	tok, err := c.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("github token: %w", err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("github api: %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &StatusError{StatusCode: resp.StatusCode, Method: http.MethodGet, URL: url, Body: strings.TrimSpace(string(body))}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return nil, fmt.Errorf("github api: %s: %w", url, err)
		}
	}
	return resp.Header, nil
}

// nextLink is the "next" URL of a Link header (RFC 5988), as GitHub
// sends for a paginated list, or "" past the last page.
func nextLink(h http.Header) string {
	for _, part := range strings.Split(h.Get("Link"), ",") {
		url, rel, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(rel, `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(url), "<>")
	}
	return ""
}
