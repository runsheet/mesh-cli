// Package latchkey speaks Latchkey's OAuth surface for the node's machine
// credential (latchkey adr-machine-identity): the RFC 8628 device flow
// `mesh node login` runs, the refresh grant that keeps its access token
// live, and the claims a machine token carries. It holds no state —
// internal/auth owns ~/.mesh/token.
package latchkey

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultIssuer is Latchkey.
const DefaultIssuer = "https://auth.latchkey.id"

// DeviceGrantType is RFC 8628's grant.
const DeviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// The OAuth error codes the device flow answers a poll with (RFC 8628 §3.5).
const (
	ErrAuthorizationPending = "authorization_pending"
	ErrSlowDown             = "slow_down"
	ErrExpiredToken         = "expired_token"
	ErrAccessDenied         = "access_denied"
)

// Client is one Latchkey issuer and the public client the CLI starts its
// flows from (runsheet's `mesh-cli` client, tagged to the org).
type Client struct {
	Issuer   string
	ClientID string
	HTTP     *http.Client
	// SlowDown is what Await adds to the poll interval each time
	// Latchkey answers slow_down; RFC 8628 §3.5 asks for five seconds.
	SlowDown time.Duration
}

// slowDown is the step Await backs off by.
func (c *Client) slowDown() time.Duration {
	if c.SlowDown > 0 {
		return c.SlowDown
	}
	return 5 * time.Second
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Error is an OAuth error response (RFC 6749 §5.2): the code is what the
// device poll dispatches on.
type Error struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
	Status      int    `json:"-"`
}

func (e *Error) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	if e.Code != "" {
		return e.Code
	}
	return fmt.Sprintf("latchkey: %d", e.Status)
}

// Code returns err's OAuth error code, or "" if it is not one.
func Code(err error) string {
	var oe *Error
	if errors.As(err, &oe) {
		return oe.Code
	}
	return ""
}

// Endpoints is what discovery tells the CLI (OpenID Provider Metadata,
// plus RFC 8628's device_authorization_endpoint).
type Endpoints struct {
	Issuer              string `json:"issuer"`
	DeviceAuthorization string `json:"device_authorization_endpoint"`
	Token               string `json:"token_endpoint"`
}

// Discover reads the issuer's OpenID configuration.
func (c *Client) Discover(ctx context.Context) (*Endpoints, error) {
	issuer := strings.TrimRight(strings.TrimSpace(c.Issuer), "/")
	if issuer == "" {
		issuer = DefaultIssuer
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("latchkey: discovery at %s: %s: %s", issuer, resp.Status, strings.TrimSpace(string(raw)))
	}
	var ep Endpoints
	if err := json.Unmarshal(raw, &ep); err != nil {
		return nil, fmt.Errorf("latchkey: decode discovery: %w", err)
	}
	if ep.Issuer == "" {
		ep.Issuer = issuer
	}
	if ep.Token == "" {
		return nil, fmt.Errorf("latchkey: %s advertises no token_endpoint", issuer)
	}
	if ep.DeviceAuthorization == "" {
		return nil, fmt.Errorf("latchkey: %s serves no device flow (no device_authorization_endpoint)", issuer)
	}
	return &ep, nil
}

// DeviceAuth is what starting the flow returns: the code the person types
// and the code the CLI polls with.
type DeviceAuth struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

// URL is the address to send the person to: the complete one when
// Latchkey gave it (the code is already in it), else the bare one.
func (d *DeviceAuth) URL() string {
	if d.VerificationURIComplete != "" {
		return d.VerificationURIComplete
	}
	return d.VerificationURI
}

// StartDevice begins the device flow. name is what the machine will be
// called in the Latchkey console (the CLI sends the hostname); the person
// may edit it on the consent page.
func (c *Client) StartDevice(ctx context.Context, ep *Endpoints, name string) (*DeviceAuth, error) {
	form := url.Values{}
	form.Set("client_id", c.ClientID)
	if strings.TrimSpace(name) != "" {
		form.Set("name", strings.TrimSpace(name))
	}
	var out DeviceAuth
	if err := c.post(ctx, ep.DeviceAuthorization, form, &out); err != nil {
		return nil, err
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, errors.New("latchkey: the device flow returned no code")
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 600
	}
	return &out, nil
}

// Tokens is a token endpoint response.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

// PollDevice asks the token endpoint once. A pending approval comes back
// as an *Error with code authorization_pending or slow_down.
func (c *Client) PollDevice(ctx context.Context, ep *Endpoints, deviceCode string) (*Tokens, error) {
	form := url.Values{}
	form.Set("grant_type", DeviceGrantType)
	form.Set("device_code", deviceCode)
	form.Set("client_id", c.ClientID)
	var out Tokens
	if err := c.post(ctx, ep.Token, form, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("latchkey: the device grant returned no access token")
	}
	return &out, nil
}

// Await polls until the person approves, refuses, or the code expires,
// honouring the interval and backing off on slow_down. note, when set, is
// called with each thing worth saying out loud.
func (c *Client) Await(ctx context.Context, ep *Endpoints, d *DeviceAuth, note func(string)) (*Tokens, error) {
	interval := time.Duration(d.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(d.ExpiresIn) * time.Second)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		toks, err := c.PollDevice(ctx, ep, d.DeviceCode)
		if err == nil {
			return toks, nil
		}
		switch Code(err) {
		case ErrAuthorizationPending:
		case ErrSlowDown:
			interval += c.slowDown()
			if note != nil {
				note(fmt.Sprintf("slow_down: polling every %s", interval))
			}
		case ErrAccessDenied:
			return nil, errors.New("latchkey: the sign-in was refused")
		case ErrExpiredToken:
			return nil, errors.New("latchkey: the code expired before it was entered; run `mesh node login` again")
		default:
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, errors.New("latchkey: the code expired before it was entered; run `mesh node login` again")
		}
	}
}

// Refresh rotates the machine credential's refresh token for a new access
// token. Latchkey rotates on every use, so the returned refresh token
// replaces the one sent.
func (c *Client) Refresh(ctx context.Context, tokenEndpoint, refreshToken string) (*Tokens, error) {
	if strings.TrimSpace(tokenEndpoint) == "" {
		return nil, errors.New("latchkey: no token endpoint on the credential")
	}
	if strings.TrimSpace(refreshToken) == "" {
		return nil, errors.New("latchkey: no refresh token on the credential")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if strings.TrimSpace(c.ClientID) != "" {
		form.Set("client_id", c.ClientID)
	}
	var out Tokens
	if err := c.post(ctx, tokenEndpoint, form, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("latchkey: the refresh grant returned no access token")
	}
	return &out, nil
}

// post sends one form and decodes the JSON body into out, turning a
// non-2xx OAuth error body into an *Error.
func (c *Client) post(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		oe := &Error{Status: resp.StatusCode}
		if err := json.Unmarshal(raw, oe); err != nil || oe.Code == "" {
			return fmt.Errorf("latchkey: %s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(raw)))
		}
		return oe
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("latchkey: decode %s: %w", endpoint, err)
	}
	return nil
}

// Claims are a machine token's claims, read without verifying the
// signature: the CLI only reports what it was given — the gateway
// verifies. The set is the one oidc/machine.go signs.
type Claims struct {
	Subject  string `json:"sub"`
	Role     string `json:"role"`
	Org      string `json:"org"`
	Name     string `json:"name"`
	JTI      string `json:"jti"`
	IssuedAt int64  `json:"iat"`
	Expiry   int64  `json:"exp"`
	Operator struct {
		Sub string `json:"sub"`
	} `json:"operator"`
}

// ParseClaims decodes a JWT's payload. It does not verify the signature.
func ParseClaims(token string) (*Claims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, errors.New("latchkey: not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("latchkey: decode claims: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("latchkey: decode claims: %w", err)
	}
	return &c, nil
}

// MachineID is the credential's id: `sub` without its `machine:` prefix,
// empty if the token is not a machine token.
func (c *Claims) MachineID() string {
	if c == nil || !strings.HasPrefix(c.Subject, "machine:") {
		return ""
	}
	return strings.TrimPrefix(c.Subject, "machine:")
}

// ExpiresAt is the token's `exp`, zero when it carries none.
func (c *Claims) ExpiresAt() time.Time {
	if c == nil || c.Expiry == 0 {
		return time.Time{}
	}
	return time.Unix(c.Expiry, 0)
}

// ClientCredentials is RFC 6749 §4.4's grant: a confidential client's own
// token, its id and secret sent as HTTP Basic. The CLI's own ClientID is
// not used; the client is the one named. A product publishes its
// catalogue to Mesh as itself this way (runsheet ADR 067 §§5–6).
func (c *Client) ClientCredentials(ctx context.Context, tokenEndpoint, clientID, secret string) (string, error) {
	if strings.TrimSpace(tokenEndpoint) == "" {
		return "", errors.New("latchkey: no token endpoint")
	}
	if strings.TrimSpace(clientID) == "" || secret == "" {
		return "", errors.New("latchkey: a client's id and secret are both needed")
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(secret))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		oe := &Error{Status: resp.StatusCode}
		if err := json.Unmarshal(raw, oe); err != nil || oe.Code == "" {
			return "", fmt.Errorf("latchkey: %s: %s: %s", tokenEndpoint, resp.Status, strings.TrimSpace(string(raw)))
		}
		return "", oe
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("latchkey: decode %s: %w", tokenEndpoint, err)
	}
	if out.AccessToken == "" {
		return "", errors.New("latchkey: the client_credentials grant returned no access token")
	}
	return out.AccessToken, nil
}
