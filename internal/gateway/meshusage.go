package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// JobUsage is what one job's run cost, reported to Mesh before the job
// ends so its end carries it (runsheet ADR 078): the account it ran on
// by reference — never the secret — the harness and model, its tokens,
// its cost at list price and as charged, its wall time and its turns.
type JobUsage struct {
	AccountRef       string `json:"account_ref"`
	Harness          string `json:"harness"`
	Model            string `json:"model"`
	TokensInput      int64  `json:"tokens_input"`
	TokensOutput     int64  `json:"tokens_output"`
	TokensCacheRead  int64  `json:"tokens_cache_read"`
	TokensCacheWrite int64  `json:"tokens_cache_write"`
	CostListCents    int64  `json:"cost_list_cents"`
	CostActualCents  int64  `json:"cost_actual_cents"`
	DurationMs       int64  `json:"duration_ms"`
	Turns            int64  `json:"turns"`
}

// JobUsageOf is r as Mesh takes a job's usage.
func JobUsageOf(r *UsageReport) JobUsage {
	return JobUsage{
		AccountRef: r.CredentialID, Harness: r.Adapter, Model: r.Model,
		TokensInput: r.InputTokens, TokensOutput: r.OutputTokens, TokensCacheRead: r.CacheReadTokens, TokensCacheWrite: r.CacheWriteTokens,
		CostListCents: r.ListCostCents, CostActualCents: r.ActualCostCents, DurationMs: r.WallMs, Turns: r.Turns,
	}
}

// AccountWindow is one limit window of an account as the harness last
// read it: the window (five_hour, seven_day), the percent used, and when
// it resets.
type AccountWindow struct {
	Window   string    `json:"window"`
	UsedPct  float64   `json:"used_pct"`
	ResetsAt time.Time `json:"resets_at,omitzero"`
}

// NodeAccount is one account's windows and overage standing, by
// reference.
type NodeAccount struct {
	AccountRef string          `json:"account_ref"`
	Overage    string          `json:"overage"`
	Windows    []AccountWindow `json:"windows"`
}

// MeshJobUsagePath and MeshNodeAccountsPath are the node's two reports
// to Mesh (runsheet ADR 078 §5).
const (
	MeshJobUsagePath     = "/mesh/v1/jobs/{id}/usage"
	MeshNodeAccountsPath = "/mesh/v1/nodes/{id}/accounts"
)

// ReportJobUsage sends a job's usage to Mesh. A platform without the
// route answers IsUnsupported: a 404 or 405 that is no JSON error of
// Mesh's own (an unknown job is a JSON 404, a refusal). The same report
// again is a no-op on the platform.
func (c *Client) ReportJobUsage(ctx context.Context, nodeID, jobID string, u JobUsage) error {
	if jobID == "" {
		return errors.New("job usage: no job")
	}
	if u.AccountRef == "" {
		return errors.New("job usage: no account_ref")
	}
	return c.postMesh(ctx, nodeID, strings.Replace(MeshJobUsagePath, "{id}", jobID, 1), u, "the platform takes no job usage")
}

// ReportNodeAccounts sends the node's accounts' windows to Mesh, each by
// reference, as read at seenAt. Unsupported as ReportJobUsage is.
func (c *Client) ReportNodeAccounts(ctx context.Context, nodeID string, accounts []NodeAccount, seenAt time.Time) error {
	if nodeID == "" {
		return errors.New("node accounts: no node")
	}
	body := map[string]any{"accounts": accounts, "seen_at": seenAt.UTC().Format(time.RFC3339)}
	return c.postMesh(ctx, nodeID, strings.Replace(MeshNodeAccountsPath, "{id}", nodeID, 1), body, "the platform takes no node accounts")
}

// postMesh posts body as JSON to path on the node's own bearer.
func (c *Client) postMesh(ctx context.Context, nodeID, path string, body any, unsupported string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json")
	if nodeID != "" {
		hr.Header.Set(NodeIDHeader, nodeID)
	}
	tok, err := c.Bearer(ctx)
	if err != nil {
		return err
	}
	hr.Header.Set("Authorization", "Bearer "+tok)
	out, status, err := c.doREST(hr)
	if err != nil {
		return err
	}
	switch {
	case (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) && !json.Valid(out):
		return &Error{Message: unsupported + " (POST " + path + ")", Unsupported: true, Status: status}
	case status >= 400 && status < 500:
		return &Error{Message: restMessage(out, status), Status: status}
	case status < 200 || status >= 300:
		return &httpError{Status: status, Text: http.StatusText(status), Body: restMessage(out, status)}
	}
	return nil
}
