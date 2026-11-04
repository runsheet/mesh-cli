package relaysidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"projectmesh.io/mesh-cli/internal/dpop"
)

// A node that adopts a lease at start (ADR-044, "a v1 node runs each
// lease as a job actor", the start-up amendment) opens the lease's
// session again from nothing: a fresh DPoP key, a fresh session token
// minted under it, a fresh git token if the job holds one. None of that
// reaches the running sidecar through RenewRequest/Renewal (renew.go's
// pair is the sidecar's own pull, near a held token's end, signed under
// the key it already has): a DPoP proof is bound to the token's minting
// key's thumbprint, so the old session token is worthless the moment
// the node mints the new one under the new key, and the sidecar must
// take the whole set at once, from the node, before anything of the
// lease asks it for either. ReplaceCredentialsPath is that push, on the
// same control socket as the push tool's PushPath: the node's own UID
// reaches it, never the session.

// ReplaceCredentialsPath is served by the sidecar, on its control
// socket, for an adopting node to replace the lease's key and tokens at
// once.
const ReplaceCredentialsPath = "/relay/replace-credentials"

// ReplaceCredentialsRequest is the adopting node's ask: the lease's new
// DPoP key, its new session token, and its new git token when the job
// holds one (nil leaves the sidecar's git token as it is, for a lease
// with none).
type ReplaceCredentialsRequest struct {
	Key     dpop.Handoff `json:"key"`
	Session Token        `json:"session"`
	Git     *Token       `json:"git,omitempty"`
}

// ReplaceCredentialsResult is the sidecar's answer: Replaced, or
// Refused with why.
type ReplaceCredentialsResult struct {
	Replaced bool   `json:"replaced"`
	Refused  string `json:"refused,omitempty"`
}

func (s *Sidecar) serveReplaceCredentials(w http.ResponseWriter, r *http.Request) {
	var req ReplaceCredentialsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, controlBodyMax)).Decode(&req); err != nil {
		http.Error(w, "not credentials", http.StatusBadRequest)
		return
	}
	res := s.ReplaceCredentials(req)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// ReplaceCredentials swaps the lease's DPoP key and session token, and
// its git token when req.Git is set, for every upstream the sidecar
// already serves: each was built with a Signer and a Bearer that read
// the sidecar's live state (keyRef, sessionToken, gitToken), so the
// swap alone reaches them without rebuilding anything.
func (s *Sidecar) ReplaceCredentials(req ReplaceCredentialsRequest) ReplaceCredentialsResult {
	key, err := dpop.KeyFromHandoff(req.Key)
	if err != nil {
		return ReplaceCredentialsResult{Refused: fmt.Sprintf("the new key: %v", err)}
	}
	if req.Session.Value == "" {
		return ReplaceCredentialsResult{Refused: "no session token"}
	}
	if req.Git != nil && s.creds.Git == nil {
		return ReplaceCredentialsResult{Refused: "this lease has no repository to hold a git token for"}
	}
	s.mu.Lock()
	s.key = key
	s.session = req.Session
	if req.Git != nil {
		s.git = *req.Git
	}
	s.mu.Unlock()
	return ReplaceCredentialsResult{Replaced: true}
}

// ReplaceCredentialsClient is the adopting node's way to its lease's
// sidecar, on the control socket.
type ReplaceCredentialsClient struct {
	// Socket is the sidecar's control socket.
	Socket string
}

// Replace asks the sidecar to take req as its lease's credentials from
// now on. An error is the sidecar not answering; a refusal is in the
// result.
func (c ReplaceCredentialsClient) Replace(ctx context.Context, req ReplaceCredentialsRequest) (ReplaceCredentialsResult, error) {
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", c.Socket)
	}}}
	b, err := json.Marshal(req)
	if err != nil {
		return ReplaceCredentialsResult{}, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://sidecar"+ReplaceCredentialsPath, bytes.NewReader(b))
	if err != nil {
		return ReplaceCredentialsResult{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(hr)
	if err != nil {
		return ReplaceCredentialsResult{}, fmt.Errorf("the lease's relay sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ReplaceCredentialsResult{}, fmt.Errorf("the lease's relay sidecar answered %d", resp.StatusCode)
	}
	var out ReplaceCredentialsResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, controlBodyMax)).Decode(&out); err != nil {
		return ReplaceCredentialsResult{}, fmt.Errorf("the lease's relay sidecar: %w", err)
	}
	return out, nil
}
