package gateway

import (
	"context"
	"errors"
	"strings"
)

// The one claim (runsheet ADR 075, one job queue, step Q2): claimJob
// naming no subject asks Mesh for the first job in queue order this node
// can run. Mesh orders the offers and tries each kind's own claim; the
// node runs what it is handed.

// ClaimNextInput is the one claim's input: the node, its harness, and
// optionally the one kind it will take ("" any).
type ClaimNextInput struct {
	NodeID  string
	Harness string
	Kind    string
}

// ClaimNextAnswer is the one claim's answer: Job when one was claimed,
// nil with Offers and Tried when none was.
type ClaimNextAnswer struct {
	Status string
	Job    *JobBrief
	Offers int
	Tried  int
}

// claimNextOld is how a platform from before the one claim refuses a
// claimJob that names no subject.
const claimNextOld = "subject names the job's subject"

// ClaimNext is the one claim. A platform from before it answers an
// unsupported *Error (IsUnsupported): the node then claims kind by kind
// as it did.
func (c *Client) ClaimNext(ctx context.Context, in ClaimNextInput) (*ClaimNextAnswer, error) {
	m := map[string]any{"nodeId": in.NodeID}
	if in.Harness != "" {
		m["harness"] = in.Harness
	}
	if in.Kind != "" {
		m["kind"] = in.Kind
	}
	var out struct {
		Answer struct {
			Status string    `json:"status"`
			Job    *JobBrief `json:"job"`
			Offers int       `json:"offers"`
			Tried  int       `json:"tried"`
		} `json:"claimJob"`
	}
	if err := c.Query(ctx, `mutation($in: Map!) { claimJob(input: $in) }`, map[string]any{"in": m}, &out); err != nil {
		var ge *Error
		if errors.As(err, &ge) && !ge.Unsupported && strings.Contains(ge.Message, claimNextOld) {
			return nil, &Error{Message: ge.Message, Unsupported: true}
		}
		return nil, err
	}
	a := &ClaimNextAnswer{Status: out.Answer.Status, Job: out.Answer.Job, Offers: out.Answer.Offers, Tried: out.Answer.Tried}
	if a.Job != nil {
		c.keepBrief(a.Job)
	}
	return a, nil
}
