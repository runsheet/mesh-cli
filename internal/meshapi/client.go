// Package meshapi is mesh-cli's typed client for the Mesh v1 node API
// (projectmesh-io/api, Mesh v1 spec "Node API"): register, heartbeat,
// take, and the lease routes — renew, session, evidence, blobs, access,
// usage, finish, fail and hand back. It speaks on the node's machine
// token and knows nothing of runsheet's gateway; internal/nodev1 is its
// one caller. Its fixtures (testdata) are the contract: each is one
// request and answer of contracts/v1, and the tests hold this client's
// JSON to them byte for byte, both ways.
package meshapi

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

// DefaultURL is Mesh's production API.
const DefaultURL = "https://api.projectmesh.io"

// MaxWaitS is the longest a take may wait (Mesh v1 spec).
const MaxWaitS = 25

// Client reaches one Mesh API.
type Client struct {
	// BaseURL is Mesh's origin; DefaultURL when empty.
	BaseURL string
	// Token answers the node's machine bearer for each request.
	Token func(ctx context.Context) (string, error)
	// HTTP is the client requests go on; a plain one when nil. A take
	// waits up to MaxWaitS on the server, so its timeout must exceed
	// that.
	HTTP *http.Client
}

// Error is a refusal from Mesh: its status, code and message.
type Error struct {
	Status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return fmt.Sprintf("mesh: %d %s: %s", e.Status, e.Code, msg)
	}
	return fmt.Sprintf("mesh: %d: %s", e.Status, msg)
}

// LeaseGone says whether err is Mesh saying the lease is no longer the
// node's: expired, handed back, cancelled or ended (404, 409 or 410). A
// node that hears it stops the lease's work and reports nothing more.
func LeaseGone(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == http.StatusNotFound || e.Status == http.StatusConflict || e.Status == http.StatusGone
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: (MaxWaitS + 20) * time.Second}
}

// do sends body (JSON, or raw when it is []byte) to path and decodes a
// 2xx answer into out when out is non-nil. A 204 decodes nothing and
// answers status 204. A non-2xx answer is an *Error.
func (c *Client) do(ctx context.Context, method, path string, body, out any) (int, http.Header, error) {
	var rd io.Reader
	ctype := "application/json"
	switch b := body.(type) {
	case nil:
	case []byte:
		rd, ctype = bytes.NewReader(b), "application/octet-stream"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if rd != nil {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != nil {
		tok, err := c.Token(ctx)
		if err != nil {
			return 0, nil, fmt.Errorf("mesh: bearer: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode}
		if json.Unmarshal(raw, e) != nil || (e.Code == "" && e.Message == "") {
			if s := strings.TrimSpace(string(raw)); len(s) < 300 {
				e.Message = s
			}
		}
		return resp.StatusCode, resp.Header, e
	}
	if out != nil && resp.StatusCode != http.StatusNoContent && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, resp.Header, fmt.Errorf("mesh: %s %s: decode: %w", method, path, err)
		}
	}
	return resp.StatusCode, resp.Header, nil
}

// Register registers the node: POST /v1/nodes.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*Node, error) {
	var out Node
	if _, _, err := c.do(ctx, http.MethodPost, "/v1/nodes", req, &out); err != nil {
		return nil, err
	}
	if out.NodeID == "" {
		return nil, errors.New("mesh: the registration answered no node_id")
	}
	return &out, nil
}

// Connect offers the node to a tenant for a product: POST
// /v1/nodes/{id}/connections. The answer says whether it was accepted.
func (c *Client) Connect(ctx context.Context, nodeID string, req ConnectRequest) (*Connection, error) {
	var out Connection
	if _, _, err := c.do(ctx, http.MethodPost, "/v1/nodes/"+url.PathEscape(nodeID)+"/connections", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Heartbeat reports the node: POST /v1/nodes/{id}/heartbeat.
func (c *Client) Heartbeat(ctx context.Context, nodeID string, req HeartbeatRequest) (*HeartbeatResponse, error) {
	if req.Leases == nil {
		req.Leases = []LeaseBeat{}
	}
	if req.Accounts == nil {
		req.Accounts = []AccountReport{}
	}
	var out HeartbeatResponse
	if _, _, err := c.do(ctx, http.MethodPost, "/v1/nodes/"+url.PathEscape(nodeID)+"/heartbeat", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WaitingHeader carries a 204 take's waiting reason: a 204 has no body.
const WaitingHeader = "Mesh-Waiting"

// Take long-polls for a lease: POST /v1/take. No job is a Take with
// Waiting set (Mesh's 204, its reason in WaitingHeader, or a 200 that
// names no lease).
func (c *Client) Take(ctx context.Context, req TakeRequest) (*Take, error) {
	if req.WaitS > MaxWaitS {
		req.WaitS = MaxWaitS
	}
	var out Take
	status, h, err := c.do(ctx, http.MethodPost, "/v1/take", req, &out)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return &Take{Waiting: h.Get(WaitingHeader)}, nil
	}
	if out.Lease == nil || out.Brief == nil {
		return &Take{Waiting: out.Waiting}, nil
	}
	return &out, nil
}

func leasePath(leaseID, route string) string {
	return "/v1/leases/" + url.PathEscape(leaseID) + "/" + route
}

// Renew extends a lease: POST /v1/leases/{id}/renew {turns_seen?}. A
// thread's lease names how far its session has the thread, and Mesh
// answers the turns above that (projectmesh-io/api ADR 001 amendment,
// threads: "Acknowledged turns"); nil says nothing of turns.
func (c *Client) Renew(ctx context.Context, leaseID string, turnsSeen *int64) (*RenewResponse, error) {
	var out RenewResponse
	if _, _, err := c.do(ctx, http.MethodPost, leasePath(leaseID, "renew"), RenewRequest{TurnsSeen: turnsSeen}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Session opens the lease's Latchkey session with Mesh as attester:
// POST /v1/leases/{id}/session {jkt}. The request credential is in the
// answer once and nowhere else.
func (c *Client) Session(ctx context.Context, leaseID, jkt string) (*SessionCredential, error) {
	var out SessionCredential
	if _, _, err := c.do(ctx, http.MethodPost, leasePath(leaseID, "session"), SessionRequest{JKT: jkt}, &out); err != nil {
		return nil, err
	}
	if out.SessionID == "" || out.RequestURL == "" || out.RequestToken == "" {
		return nil, errors.New("mesh: the session answer names no session, request url or request token")
	}
	return &out, nil
}

// Evidence records the lease's signed ToolRecords: POST
// /v1/leases/{id}/evidence. Mesh answers each record's evidence id.
func (c *Client) Evidence(ctx context.Context, leaseID string, records []EvidencePost) (*EvidenceResponse, error) {
	var out EvidenceResponse
	if _, _, err := c.do(ctx, http.MethodPost, leasePath(leaseID, "evidence"), EvidenceRequest{Records: records}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PutBlob uploads one evidence blob by its digest ("sha256:<hex>" of
// its bytes): PUT /v1/blobs/{digest}. A blob Mesh holds already is
// answered the same.
func (c *Client) PutBlob(ctx context.Context, digest string, data []byte) error {
	_, _, err := c.do(ctx, http.MethodPut, "/v1/blobs/"+digest, data, nil)
	return err
}

// Access posts the relay's AccessRecords: POST /v1/leases/{id}/access.
func (c *Client) Access(ctx context.Context, leaseID string, records []AccessRecord) error {
	_, _, err := c.do(ctx, http.MethodPost, leasePath(leaseID, "access"), AccessRequest{Records: records}, nil)
	return err
}

// Security posts what the lease's relay saw, 1 to MaxSecurityReports
// reports, for Mesh to forward to Tripline: POST
// /v1/leases/{id}/security. A lease may report after it ended. A 422 is
// final; a 5xx is worth sending again.
func (c *Client) Security(ctx context.Context, leaseID string, reports []SecurityReport) (*SecurityAccepted, error) {
	var out SecurityAccepted
	if _, _, err := c.do(ctx, http.MethodPost, leasePath(leaseID, "security"), SecurityRequest{Reports: reports}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Usage posts what the lease spent: POST /v1/leases/{id}/usage.
func (c *Client) Usage(ctx context.Context, leaseID string, req UsageRequest) error {
	_, _, err := c.do(ctx, http.MethodPost, leasePath(leaseID, "usage"), req, nil)
	return err
}

// Finish ends the job with the product's result: POST
// /v1/leases/{id}/finish.
func (c *Client) Finish(ctx context.Context, leaseID string, req FinishRequest) (*JobState, error) {
	if len(req.Result) == 0 {
		req.Result = json.RawMessage("{}")
	}
	return c.end(ctx, leaseID, "finish", req)
}

// Fail ends the job as a work failure: POST /v1/leases/{id}/fail.
func (c *Client) Fail(ctx context.Context, leaseID string, req FailRequest) (*JobState, error) {
	if req.Cause == "" {
		req.Cause = CauseWork
	}
	return c.end(ctx, leaseID, "fail", req)
}

// HandBack ends the lease and offers the job again: POST
// /v1/leases/{id}/hand-back.
func (c *Client) HandBack(ctx context.Context, leaseID string, req HandBackRequest) (*JobState, error) {
	if req.Cause == "" {
		return nil, errors.New("mesh: a hand-back needs a cause")
	}
	return c.end(ctx, leaseID, "hand-back", req)
}

func (c *Client) end(ctx context.Context, leaseID, route string, body any) (*JobState, error) {
	var out JobState
	if _, _, err := c.do(ctx, http.MethodPost, leasePath(leaseID, route), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
