package meshapi

import (
	"context"
	"net/http"
	"time"
)

// ProgressRequest is a lease's progress note: one line of what its
// session has done (projectmesh-io/api ADR 001 amendment, node tools).
type ProgressRequest struct {
	Note string `json:"note"`
}

// ProgressNote is a lease's newest note as Mesh keeps it.
type ProgressNote struct {
	LeaseID string     `json:"lease_id"`
	Note    string     `json:"note"`
	At      *time.Time `json:"at,omitempty"`
	Notes   int64      `json:"notes"`
}

// Progressed is the progress route's answer: the job, its status and the
// note as the lease now keeps it.
type Progressed struct {
	JobID    string        `json:"job_id"`
	Status   string        `json:"status"`
	Progress *ProgressNote `json:"progress"`
}

// Progress posts the lease's session's note: POST
// /v1/leases/{id}/progress {note}. Mesh keeps it on the lease's
// LeaseProgress record, which the product reads with its job tool; a
// lease no longer the job's answers 409 lease_not_live (LeaseGone).
func (c *Client) Progress(ctx context.Context, leaseID string, req ProgressRequest) (*Progressed, error) {
	var out Progressed
	if _, _, err := c.do(ctx, http.MethodPost, leasePath(leaseID, "progress"), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
