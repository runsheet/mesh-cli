package gateway

// A person's thread with a product (runsheet thread.loom): what `mesh
// chat` says and reads, and the turns a node answers as jobs (ADR 061
// §7, runsheet/goal-chat).

import (
	"context"
	"fmt"
	"time"
)

// ThreadTurn is a raised turn a node may claim: a ThreadSessionIndex row.
type ThreadTurn struct {
	ID        string `json:"id"`
	Product   string `json:"product"`
	ThreadKey string `json:"threadKey"`
	OrgID     string `json:"orgId"`
	ProjectID string `json:"projectId"`
	Role      string `json:"role"`
	Status    string `json:"status"`
}

// KindIn is the turn's job kind as the platform claims it (gateway
// threadKindOf): <product>/<role> when kinds, the catalogue, declares
// it, else the product's thread kind, <product>/thread.
func (t ThreadTurn) KindIn(kinds map[string]JobKind) string {
	if t.Role != "" {
		if _, ok := kinds[t.Product+"/"+t.Role]; ok {
			return t.Product + "/" + t.Role
		}
	}
	return t.Product + "/thread"
}

// RaisedThreadTurns are an org's turns waiting for a node, product's
// alone when product is set.
func (c *Client) RaisedThreadTurns(ctx context.Context, orgID, product string) ([]ThreadTurn, error) {
	var out struct {
		Turns []ThreadTurn `json:"threadSessionIndexs"`
	}
	wh := where("org_id", orgID) + ", " + where("status", "raised")
	if product != "" {
		wh += ", " + where("product", product)
	}
	q := fmt.Sprintf(`{ threadSessionIndexs(namespace: %q, where: [%s]) { %s } }`, NS, wh, threadSessionFields)
	if err := c.Query(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	return out.Turns, nil
}

// SayInThread is a person's message on the thread of product and key
// (ids.ThreadID), starting it in org and project with role when it is
// new.
type SayInThread struct {
	ThreadID, Product, ThreadKey, OrgID, ProjectID, Role, Body, By string
}

// SayInThread says one thing on a thread, in the caller's name.
func (c *Client) SayInThread(ctx context.Context, in SayInThread) error {
	m := map[string]any{"aggregateId": in.ThreadID, "namespace": NS, "product": in.Product, "threadKey": in.ThreadKey,
		"orgId": in.OrgID, "role": in.Role, "body": in.Body, "by": in.By, "saidAt": time.Now().UTC().Format(time.RFC3339Nano)}
	if in.ProjectID != "" {
		m["projectId"] = in.ProjectID
	}
	var out map[string]any
	return c.Query(ctx, `mutation($in: SayInThreadInput!) { sayInThread(input: $in) { status } }`, map[string]any{"in": m}, &out)
}

// ThreadMessage is one message of a thread: the person's or the role's
// (from agent), its proposal and the goal it became.
type ThreadMessage struct {
	Seq      int64  `json:"seq"`
	From     string `json:"from"`
	By       string `json:"by,omitempty"`
	Body     string `json:"body"`
	Proposal string `json:"proposal,omitempty"`
	GoalID   string `json:"goalId,omitempty"`
}

// Thread is a thread as threadMessages answers it.
type Thread struct {
	ThreadID  string          `json:"threadId"`
	Product   string          `json:"product"`
	ThreadKey string          `json:"threadKey"`
	OrgID     string          `json:"orgId"`
	ProjectID string          `json:"projectId,omitempty"`
	Role      string          `json:"role"`
	Status    string          `json:"status"`
	Messages  []ThreadMessage `json:"messages"`
}

// Waiting says the role has not answered the last message.
func (t *Thread) Waiting() bool { return t.Status == "waiting" }

// ThreadMessages reads a thread: its messages, to anyone on its org.
func (c *Client) ThreadMessages(ctx context.Context, threadID string) (*Thread, error) {
	var out struct {
		Thread *Thread `json:"threadMessages"`
	}
	if err := c.Query(ctx, `query($in: Map!) { threadMessages(input: $in) }`, map[string]any{"in": map[string]any{"threadId": threadID}}, &out); err != nil {
		return nil, err
	}
	if out.Thread == nil {
		return nil, fmt.Errorf("thread %s: no answer", threadID)
	}
	return out.Thread, nil
}

// RecordThreadProposalAccepted records that the goal goalID is what
// message seq of the thread proposed, set by the caller.
func (c *Client) RecordThreadProposalAccepted(ctx context.Context, threadID string, seq int64, goalID, by string) error {
	var out map[string]any
	return c.Query(ctx, `mutation($in: RecordThreadProposalAcceptedInput!) { recordThreadProposalAccepted(input: $in) { status } }`, map[string]any{"in": map[string]any{
		"aggregateId": threadID, "namespace": NS, "seq": seq, "goalId": goalID, "by": by, "acceptedAt": time.Now().UTC().Format(time.RFC3339Nano)}}, &out)
}
