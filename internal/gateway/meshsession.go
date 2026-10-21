package gateway

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
	"time"
)

// A mesh session is runsheet's record of one build claim (runsheet ADR
// 056, the platform's half of Latchkey ADR 013's session tokens): the
// node opens it once it holds the claim and before the harness starts,
// on the credential it claimed with, and is answered a request bearer
// (`rsr_…`) bound to that claim alone. The bearer mints the session's
// own id tokens at the request URL — the token /mcp takes for that
// session, narrowed to its item's project — and dies with the claim.
// The node never hands the session its own machine bearer when it has
// one (mesh-cli ADR-035).

// MeshSessionsPath is the gateway's route that opens a mesh session.
const MeshSessionsPath = "/mesh/v1/sessions"

// NodeIDHeader names the node a request speaks for: one machine
// credential may stand for several node ids, one per served org, so the
// claim-time read and a session's open say which.
const NodeIDHeader = "X-Mesh-Node-Id"

// MeshSessionRequest opens the mesh session of one claim: the item, the
// claim the node holds (its claim seq; 0 when the node does not know it,
// left for the platform to read) and the node holding it.
type MeshSessionRequest struct {
	ItemID   string `json:"item_id"`
	ClaimSeq int    `json:"claim_seq,omitempty"`
	NodeID   string `json:"node_id,omitempty"`
}

// MeshSession is the platform's answer: the session's id, the URL its
// id tokens are minted at and the request bearer that mints them, never
// logged or written to a row (String names the session alone).
type MeshSession struct {
	SessionID    string    `json:"session_id"`
	RequestURL   string    `json:"request_url"`
	RequestToken string    `json:"request_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (s *MeshSession) String() string {
	if s == nil {
		return "no mesh session"
	}
	return "mesh session " + s.SessionID
}

// OpenMeshSession opens the mesh session of req's claim: POST
// /mesh/v1/sessions on the node's own bearer. A platform without the
// route (404/405) answers IsUnsupported; a refusal — not the node
// holding the item, a person's claim, an ended session — is a *Error
// with the platform's message; anything else (5xx, transport) an error
// that is neither.
func (c *Client) OpenMeshSession(ctx context.Context, req MeshSessionRequest) (*MeshSession, error) {
	if req.ItemID == "" {
		return nil, errors.New("mesh session: no item")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+MeshSessionsPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json")
	if req.NodeID != "" {
		hr.Header.Set(NodeIDHeader, req.NodeID)
	}
	tok, err := c.Bearer(ctx)
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Authorization", "Bearer "+tok)
	raw, status, err := c.doREST(hr)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return nil, &Error{Message: "the platform opens no mesh sessions (POST " + MeshSessionsPath + ")", Unsupported: true, Status: status}
	case status >= 400 && status < 500:
		return nil, &Error{Message: "mesh session: " + restMessage(raw, status), Status: status}
	case status < 200 || status >= 300:
		return nil, &httpError{Status: status, Text: http.StatusText(status), Body: restMessage(raw, status)}
	}
	var out MeshSession
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("mesh session: decode: %w", err)
	}
	if out.SessionID == "" || out.RequestURL == "" || out.RequestToken == "" {
		return nil, errors.New("mesh session: the answer names no session, request url or request token")
	}
	return &out, nil
}

// Prover signs a request's DPoP proof (internal/dpop.Key): method and
// URL are the request's, token the access token it carries ("" at mint),
// nonce the server's DPoP-Nonce ("" when it asked for none).
type Prover interface {
	ProofWithNonce(method, rawURL, token, nonce string) (string, error)
}

// MeshSessionToken mints one id token of s for audience — mesh-session,
// the token runsheet's /mcp takes (runsheet ADR 064) — at s's request
// URL on its request bearer: the session's own bearer, which /mcp
// narrows to the session's project and refuses once the claim ends.
// With key the mint carries a DPoP proof and the token is bound to the
// key (cnf.jkt, Latchkey ADR 013's amendment): a server asking for a
// nonce (use_dpop_nonce, its DPoP-Nonce header) is asked once more with
// it. It answers the token, how long it lives and whether it is bound.
// A refusal (401, 403: the lease is over) is a *Error; a 503 (session
// tokens unavailable) an error that is not.
func (c *Client) MeshSessionToken(ctx context.Context, s *MeshSession, audience string, key Prover) (string, time.Duration, bool, error) {
	if s == nil || s.RequestURL == "" || s.RequestToken == "" {
		return "", 0, false, errors.New("mesh session: no request url")
	}
	u, err := url.Parse(s.RequestURL)
	if err != nil {
		return "", 0, false, fmt.Errorf("mesh session %s: request url: %w", s.SessionID, err)
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()
	mint := func(nonce string) ([]byte, int, http.Header, error) {
		hr, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, 0, nil, err
		}
		hr.Header.Set("Accept", "application/json")
		hr.Header.Set("Authorization", "Bearer "+s.RequestToken)
		if key != nil {
			proof, err := key.ProofWithNonce(http.MethodGet, u.String(), "", nonce)
			if err != nil {
				return nil, 0, nil, fmt.Errorf("mesh session %s: %w", s.SessionID, err)
			}
			hr.Header.Set("DPoP", proof)
		}
		return c.doRESTHeaders(hr)
	}
	raw, status, h, err := mint("")
	if err != nil {
		return "", 0, false, err
	}
	if nonce := h.Get("DPoP-Nonce"); key != nil && nonce != "" && (status == http.StatusBadRequest || status == http.StatusUnauthorized) {
		if raw, status, _, err = mint(nonce); err != nil {
			return "", 0, false, err
		}
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusConflict:
		return "", 0, false, &Error{Message: fmt.Sprintf("mesh session %s: %s", s.SessionID, restMessage(raw, status)), Status: status}
	case status < 200 || status >= 300:
		return "", 0, false, &httpError{Status: status, Text: http.StatusText(status), Body: restMessage(raw, status)}
	}
	var out struct {
		Value     string `json:"value"`
		ExpiresIn int    `json:"expires_in"`
		TokenType string `json:"token_type"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, false, fmt.Errorf("mesh session %s: decode: %w", s.SessionID, err)
	}
	if out.Value == "" {
		return "", 0, false, fmt.Errorf("mesh session %s: the request url answered no token", s.SessionID)
	}
	bound := strings.EqualFold(out.TokenType, "DPoP")
	if key != nil && !bound {
		return "", 0, false, fmt.Errorf("mesh session %s: the mint carried a DPoP proof but answered an unbound token", s.SessionID)
	}
	return out.Value, time.Duration(out.ExpiresIn) * time.Second, bound, nil
}

// doRESTHeaders is doREST answering the response's headers too.
func (c *Client) doRESTHeaders(req *http.Request) ([]byte, int, http.Header, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, nil, err
	}
	return raw, resp.StatusCode, resp.Header, nil
}

// doREST sends one request and answers its body and status; only a
// transport failure is an error.
func (c *Client) doREST(req *http.Request) ([]byte, int, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, err
	}
	return raw, resp.StatusCode, nil
}

// restMessage is a REST answer's `message`, else its text, else the
// status's.
func restMessage(raw []byte, status int) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &m) == nil && m.Message != "" {
		return m.Message
	}
	if s := strings.TrimSpace(string(raw)); s != "" && len(s) < 300 {
		return s
	}
	return http.StatusText(status)
}

type nodeIDKey struct{}

// withNodeID marks ctx's GraphQL requests as speaking for nodeID
// (NodeIDHeader).
func withNodeID(ctx context.Context, nodeID string) context.Context {
	if nodeID == "" {
		return ctx
	}
	return context.WithValue(ctx, nodeIDKey{}, nodeID)
}

func nodeIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(nodeIDKey{}).(string)
	return s
}
