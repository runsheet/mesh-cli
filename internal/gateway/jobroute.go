package gateway

// The node speaks the job system (runsheet ADR 061 §3): every claim and
// every finish of runsheet's kinds goes through claimJob and finishJob,
// the one claim and the one finish, whatever older mutation the node's
// code names. MutateFields routes them here: a claim is the kind and
// subject's claimJob; a finish is finishJob with the older mutation's
// input as the value of the kind's finish key. The platform routes each
// to the command the older mutation sent, so its refusals, and the
// input's every field, are as they were.

import (
	"context"
	"encoding/json"
	"fmt"
)

// jobRoute is an older mutation as a job: its kind, its subject's type,
// and — for a finish — the key the kind's finish shape carries its input
// under ("" for a claim).
type jobRoute struct {
	kind, subject, finish string
}

// jobRoutes are the older claims and finishes of runsheet's kinds.
var jobRoutes = map[string]jobRoute{
	"claimWorkItem":               {"runsheet/build", "item", ""},
	"claimWorkItemEvaluation":     {"runsheet/evaluate", "item", ""},
	"claimWorkItemRebase":         {"runsheet/rebase", "item", ""},
	"claimPlan":                   {"runsheet/plan", "goal", ""},
	"claimDirectionResearch":      {"runsheet/research", "direction", ""},
	"claimMergeBatchValidation":   {"runsheet/validate", "candidate", ""},
	"claimWorkItemMainValidation": {"runsheet/validate", "main", ""},
	"finishWorkItem":              {"runsheet/build", "item", "changeset"},
	"evaluateWorkItem":            {"runsheet/evaluate", "item", "verdict"},
	"planGoal":                    {"runsheet/plan", "goal", "plan"},
	"recordDirectionResearch":     {"runsheet/research", "direction", "research"},
	"finishWorkItemRebase":        {"runsheet/rebase", "item", "rebasedHead"},
}

// WithNodeID marks ctx's requests as the node nodeID's: the node a
// finish is handed back by (finishJob's nodeId) and the one a claim-time
// read speaks for (NodeIDHeader).
func WithNodeID(ctx context.Context, nodeID string) context.Context { return withNodeID(ctx, nodeID) }

// routeJob sends the older mutation field as its job call, in as
// MutateFields built it (aggregateId, namespace and the actor set). It
// reports false when field is no job's.
func (c *Client) routeJob(ctx context.Context, field string, in map[string]any) (bool, error) {
	r, ok := jobRoutes[field]
	if !ok {
		return false, nil
	}
	id, _ := in["aggregateId"].(string)
	subject := JobSubject{Type: r.subject, ID: id}
	node, _ := in["nodeId"].(string)
	if node == "" {
		node = nodeIDFrom(ctx)
	}
	if r.finish == "" {
		harness, _ := in["harness"].(string)
		actor, _ := in[ActorField].(string)
		_, err := c.ClaimJob(ctx, ClaimJobInput{NodeID: node, Kind: r.kind, Subject: subject, Harness: harness, Actor: actor})
		return true, err
	}
	value := make(map[string]any, len(in))
	for k, v := range in {
		if k != "aggregateId" && k != "namespace" {
			value[k] = v
		}
	}
	if node == "" {
		return true, fmt.Errorf("%s: finishJob names the node that finishes (gateway.WithNodeID)", field)
	}
	_, err := c.FinishJob(ctx, node, r.kind, subject, map[string]any{r.finish: value})
	return true, err
}

type jobKindKey struct{}

// WithJobKind marks ctx's claim-time reads as the job kind's (runsheet
// ADR 061 §4): an item's repository token, accounts and tools are the
// session the kind names — a build's write token, an evaluation's read
// one. It starts a session start's exchange: the reads under ctx share
// one jobAccess (JobAccess).
func WithJobKind(ctx context.Context, kind string) context.Context {
	if kind == "" {
		return ctx
	}
	ctx = context.WithValue(ctx, exchangesKey{}, &exchanges{})
	return context.WithValue(ctx, jobKindKey{}, kind)
}

func jobKindFrom(ctx context.Context) string {
	s, _ := ctx.Value(jobKindKey{}).(string)
	return s
}

// jobRepoToken is the job's one repository token from the exchange: an
// entry the platform did not serve is its refusal, naming why.
func (c *Client) jobRepoToken(ctx context.Context, kind string, subject JobSubject) (*GitHubToken, error) {
	acc, err := c.JobAccess(ctx, kind, subject, "")
	if err != nil {
		return nil, err
	}
	if why := acc.Missing("repo"); why != "" {
		return nil, &Error{Message: why}
	}
	if subject.Type == "candidate" {
		var one jobRepoTokenRow
		if json.Unmarshal(acc.Repo, &one) == nil && one.MergeBatchID != "" && one.MergeBatchID != subject.ID {
			return nil, fmt.Errorf("jobAccess: asked for merge batch %s, answered for %s", subject.ID, one.MergeBatchID)
		}
	}
	tok, err := acc.RepoToken()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, fmt.Errorf("jobAccess: a %s job was served no repository token", kind)
	}
	return tok, nil
}

// jobRepoTokens are a goal's or a direction's tokens from the exchange.
func (c *Client) jobRepoTokens(ctx context.Context, kind string, subject JobSubject) ([]GitHubToken, error) {
	acc, err := c.JobAccess(ctx, kind, subject, "")
	if err != nil {
		return nil, err
	}
	if why := acc.Missing("repo"); why != "" {
		return nil, &Error{Message: why}
	}
	return acc.RepoTokens()
}

// jobRepoTokenRow is the one field of a repository token the exchange's
// caller checks before it is read: the batch it was minted for.
type jobRepoTokenRow struct {
	MergeBatchID string `json:"mergeBatchId"`
}
