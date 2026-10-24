package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// A merge batch candidate is validated by a node (runsheet ADR 048
// decision 3, amended 2026-10-13; ADR 054's amendment of the same day):
// the platform serves what waits on mainValidations, a node claims one,
// runs the repository's suites on the candidate sha and attests it, and
// finishes or fails the validation. A merged item's main validation —
// the main guard's run on a merge commit — is the WorkItem twin of the
// same three commands. The contracts are specs/contracts/
// mainValidations.json and its seven siblings.

// MainValidationBatch is one merge batch candidate waiting on a node:
// mainValidations' batches[] entry.
type MainValidationBatch struct {
	MergeBatchID    string    `json:"mergeBatchId"`
	Number          int64     `json:"number"`
	CandidateBranch string    `json:"candidateBranch"`
	SHA             string    `json:"sha"`
	Attempt         int64     `json:"attempt"`
	RepoFullName    string    `json:"repoFullName"`
	RepoID          string    `json:"repoId"`
	OrgID           string    `json:"orgId"`
	DefaultBranch   string    `json:"defaultBranch"`
	RequestedAt     time.Time `json:"requestedAt"`
}

// MainValidationRequest is one merged item's merge commit waiting on a
// node: mainValidations' requests[] entry (ADR 054 decision 3, the main
// guard's run on a node).
type MainValidationRequest struct {
	WorkItemID    string    `json:"workItemId"`
	GoalID        string    `json:"goalId"`
	Seq           int64     `json:"seq"`
	SHA           string    `json:"sha"`
	Attempt       int64     `json:"attempt"`
	RepoFullName  string    `json:"repoFullName"`
	RepoID        string    `json:"repoId"`
	OrgID         string    `json:"orgId"`
	DefaultBranch string    `json:"defaultBranch"`
	RequestedAt   time.Time `json:"requestedAt"`
}

// MainValidationPullRequest is one pull request a person opened by hand
// on an enrolled repository declaring .runsheet/validate.yml, its head
// waiting on a node: mainValidations' pullRequests[] entry. Ref is
// refs/pull/<number>/head on the base repository — the head may live on
// a fork — and SHA the head the platform asks to be attested; HeadBranch
// is the pull request's head branch, for logs only.
type MainValidationPullRequest struct {
	PullRequestValidationID string    `json:"pullRequestValidationId"`
	Number                  int64     `json:"number"`
	OrgID                   string    `json:"orgId"`
	RepoID                  string    `json:"repoId"`
	RepoFullName            string    `json:"repoFullName"`
	DefaultBranch           string    `json:"defaultBranch"`
	HeadBranch              string    `json:"headBranch"`
	Ref                     string    `json:"ref"`
	SHA                     string    `json:"sha"`
	RequestedAt             time.Time `json:"requestedAt"`
	BudgetSeconds           int64     `json:"budgetSeconds"`
}

// MainValidationQueue is mainValidations' answer: every batch candidate,
// merged item and hand-opened pull request waiting on a node, across
// every org the node serves. PullRequests is empty on a platform before
// pull request validation.
type MainValidationQueue struct {
	Batches      []MainValidationBatch       `json:"batches"`
	Requests     []MainValidationRequest     `json:"requests"`
	PullRequests []MainValidationPullRequest `json:"pullRequests"`
}

// BatchAttestationReceipt is recordValidationAttestation's answer for a
// batch candidate: the platform's receipt, the candidate branch it
// names, and the whole map as sent back.
type BatchAttestationReceipt struct {
	Status         string
	Branch         string
	Commit         string
	TestEvidenceID string
	Raw            map[string]any
}

// mainValidationFields are what TakesMainValidation asks the schema for:
// the query on Query, the six commands on Mutation.
var (
	mainValidationQueryFields    = []string{"mainValidations"}
	mainValidationMutationFields = []string{
		"claimMergeBatchValidation", "finishMergeBatchValidation", "failMergeBatchValidation",
		"claimWorkItemMainValidation", "finishWorkItemMainValidation", "failWorkItemMainValidation",
	}
)

// TakesMainValidation is whether the gateway serves a node's merge
// batch and main validations — mainValidations on Query and the six
// claim, finish and fail commands on Mutation — asked by introspection
// and kept for ScalingProbeEvery, as TakesEvaluationClaim is. A failed
// probe is a no, kept as long. Every method below asks it first and,
// on a no, sends nothing and returns an Unsupported *Error.
func (c *Client) TakesMainValidation(ctx context.Context) bool {
	p := &c.mainValidationSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes
	}
	p.at, p.takes = now, false
	if !c.typeHasFields(ctx, "Query", mainValidationQueryFields) ||
		!c.typeHasFields(ctx, "Mutation", mainValidationMutationFields) {
		return false
	}
	p.takes = true
	return true
}

// pullRequestValidationMutationFields are what
// TakesPullRequestValidation asks Mutation for beside the main
// validation fields: the claim and the fail (the finish is finishJob's).
var pullRequestValidationMutationFields = []string{"claimPullRequestValidation", "failPullRequestValidation"}

// TakesPullRequestValidation is whether the gateway serves a node's
// validation of a hand-opened pull request: TakesMainValidation, and
// claimPullRequestValidation and failPullRequestValidation on Mutation,
// asked by introspection apart from it and kept for ScalingProbeEvery,
// so a platform without them still serves batches and requests. A
// failed probe is a no, kept as long.
func (c *Client) TakesPullRequestValidation(ctx context.Context) bool {
	if !c.TakesMainValidation(ctx) {
		return false
	}
	p := &c.pullRequestValidationSchema
	p.mu.Lock()
	defer p.mu.Unlock()
	now := c.now()
	if !p.at.IsZero() && now.Sub(p.at) < ScalingProbeEvery {
		return p.takes
	}
	p.at = now
	p.takes = c.typeHasFields(ctx, "Mutation", pullRequestValidationMutationFields)
	return p.takes
}

// typeHasFields is whether the named type's introspection lists every
// one of names; a failed read is a no.
func (c *Client) typeHasFields(ctx context.Context, typ string, names []string) bool {
	var out struct {
		Type *struct {
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
		} `json:"__type"`
	}
	q := fmt.Sprintf(`{ __type(name: %q) { fields { name } } }`, typ)
	if err := c.Query(ctx, q, nil, &out); err != nil || out.Type == nil {
		return false
	}
	have := map[string]bool{}
	for _, f := range out.Type.Fields {
		have[f.Name] = true
	}
	for _, n := range names {
		if !have[n] {
			return false
		}
	}
	return true
}

// errNoMainValidation is what every main-validation method returns on a
// platform without the fields: nothing was sent.
func errNoMainValidation() error {
	return &Error{Message: "gateway: the platform serves no mainValidations", Unsupported: true}
}

// MainValidations reads what waits on nodeID: `query($node: UUID!) {
// mainValidations(nodeId: $node) }`, a Map answered as batches[] and
// requests[] together (specs/contracts/mainValidations.json). A read,
// it retries a blip (retryRead).
func (c *Client) MainValidations(ctx context.Context, nodeID string) (MainValidationQueue, error) {
	var queue MainValidationQueue
	if !c.TakesMainValidation(ctx) {
		return queue, errNoMainValidation()
	}
	var out struct {
		MainValidations *MainValidationQueue `json:"mainValidations"`
	}
	q := `query($node: UUID!) { mainValidations(nodeId: $node) }`
	err := retryRead(ctx, func(ctx context.Context) error {
		return c.Query(ctx, q, map[string]any{"node": nodeID}, &out)
	})
	if err != nil {
		return queue, err
	}
	if out.MainValidations != nil {
		queue = *out.MainValidations
	}
	return queue, nil
}

// ClaimMergeBatchValidation takes a batch candidate's validation: the
// same node again converges, another node holding it, or a batch not
// waiting on a node, is refused with a gateway.Error (IsRefusal).
// harness is the job's harness, "" for the kind's own: runsheet's
// validate kind runs none, and claimJob refuses a claim naming an agent
// adapter such as claude-code.
func (c *Client) ClaimMergeBatchValidation(ctx context.Context, batchID, nodeID, harness string) error {
	return c.mainValidationCommand(ctx, "claimMergeBatchValidation", batchID, withHarness(map[string]any{"nodeId": nodeID}, harness))
}

// FailMergeBatchValidation gives the claim back with why the node could
// not validate the candidate — never a red verdict, which an
// attestation carries.
func (c *Client) FailMergeBatchValidation(ctx context.Context, batchID, nodeID, reason string) error {
	return c.mainValidationCommand(ctx, "failMergeBatchValidation", batchID, map[string]any{"nodeId": nodeID, "reason": reason})
}

// ClaimWorkItemMainValidation takes a merged item's main validation, as
// ClaimMergeBatchValidation takes a batch's.
func (c *Client) ClaimWorkItemMainValidation(ctx context.Context, itemID, nodeID, harness string) error {
	return c.mainValidationCommand(ctx, "claimWorkItemMainValidation", itemID, withHarness(map[string]any{"nodeId": nodeID}, harness))
}

// FailWorkItemMainValidation gives a merged item's main validation back
// with the reason.
func (c *Client) FailWorkItemMainValidation(ctx context.Context, itemID, reason string) error {
	return c.mainValidationCommand(ctx, "failWorkItemMainValidation", itemID, map[string]any{"reason": reason})
}

// ClaimPullRequestValidation takes a hand-opened pull request's
// validation, as ClaimMergeBatchValidation takes a batch's: the same
// node again converges, another node holding it is refused (IsRefusal).
func (c *Client) ClaimPullRequestValidation(ctx context.Context, validationID, nodeID, harness string) error {
	return c.pullRequestValidationCommand(ctx, "claimPullRequestValidation", validationID, withHarness(map[string]any{"nodeId": nodeID}, harness))
}

// FailPullRequestValidation gives a pull request validation's claim back
// with why the node could not validate the head — never a red verdict,
// which an attestation carries.
func (c *Client) FailPullRequestValidation(ctx context.Context, validationID, nodeID, reason string) error {
	return c.pullRequestValidationCommand(ctx, "failPullRequestValidation", validationID, map[string]any{"nodeId": nodeID, "reason": reason})
}

func (c *Client) pullRequestValidationCommand(ctx context.Context, field, aggregateID string, input map[string]any) error {
	if !c.TakesPullRequestValidation(ctx) {
		return &Error{Message: "gateway: the platform validates no pull request", Unsupported: true}
	}
	return c.Mutate(ctx, field, aggregateID, input)
}

func (c *Client) mainValidationCommand(ctx context.Context, field, aggregateID string, input map[string]any) error {
	if !c.TakesMainValidation(ctx) {
		return errNoMainValidation()
	}
	return c.Mutate(ctx, field, aggregateID, input)
}

func withHarness(in map[string]any, harness string) map[string]any {
	if harness != "" {
		in["harness"] = harness
	}
	return in
}

// RecordBatchCandidateAttestation uploads a batch candidate's
// validation run for the platform to sign: the same body an item's
// attestation sends (RecordValidationAttestation) with mergeBatchId
// added inside the input map, and the batch named as the call's
// mergeBatchId argument as well, `mutation($id: UUID!, $in: Map!) {
// recordValidationAttestation(mergeBatchId: $id, input: $in) }`
// (specs/contracts/recordValidationAttestation-batchCandidate.json).
// The answer names the candidate branch. A transport failure or a
// 429/5xx retries as an item's upload does (retryOnTransient).
func (c *Client) RecordBatchCandidateAttestation(ctx context.Context, batchID string, att ValidationAttestation) (BatchAttestationReceipt, error) {
	var receipt BatchAttestationReceipt
	if !c.TakesMainValidation(ctx) {
		return receipt, errNoMainValidation()
	}
	body, err := json.Marshal(att)
	if err != nil {
		return receipt, err
	}
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return receipt, err
	}
	in["mergeBatchId"] = batchID
	var out struct {
		Receipt map[string]any `json:"recordValidationAttestation"`
	}
	q := `mutation($id: UUID!, $in: Map!) { recordValidationAttestation(mergeBatchId: $id, input: $in) }`
	err = retryOnTransient(ctx, func() error {
		return c.Query(ctx, q, map[string]any{"id": batchID, "in": in}, &out)
	})
	if err != nil {
		return receipt, err
	}
	str := func(k string) string { s, _ := out.Receipt[k].(string); return s }
	return BatchAttestationReceipt{
		Status:         str("status"),
		Branch:         str("branch"),
		Commit:         str("commit"),
		TestEvidenceID: str("testEvidenceId"),
		Raw:            out.Receipt,
	}, nil
}

// FinishValidationJob is a validate job's finish (runsheet ADR 061 §7):
// the attestation of a candidate (subject candidate, id the merge
// batch's), of a merged item's merge on main (subject main, id the
// item's) or of a hand-opened pull request's head (subject
// pull_request, id the pull request validation's), handed back once
// through finishJob, which records it and ends the claim. It answers
// the attestation's receipt.
func (c *Client) FinishValidationJob(ctx context.Context, subject, id, nodeID string, att ValidationAttestation) (map[string]any, error) {
	body, err := json.Marshal(att)
	if err != nil {
		return nil, err
	}
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	if subject == "candidate" {
		in["mergeBatchId"] = id
	}
	var out map[string]any
	err = retryOnTransient(ctx, func() error {
		var err error
		out, err = c.FinishJob(ctx, nodeID, "runsheet/validate", JobSubject{Type: subject, ID: id}, map[string]any{"attestation": in})
		return err
	})
	if err != nil {
		return nil, err
	}
	answers, _ := out["answers"].(map[string]any)
	receipt, _ := answers["recordValidationAttestation"].(map[string]any)
	return receipt, nil
}

// MainValidationToken is a merged item's main validation's read token
// of its repository, from the exchange (jobAccess, runsheet ADR 061 §4):
// what its fetches carry instead of the operator's login.
func (c *Client) MainValidationToken(ctx context.Context, itemID, nodeID string) (*GitHubToken, error) {
	return c.jobRepoToken(withNodeID(ctx, nodeID), "runsheet/validate", JobSubject{Type: "main", ID: itemID})
}

// PullRequestValidationToken is a hand-opened pull request's validation's
// read token of its repository, from the exchange (jobAccess, subject
// pull_request): what its fetches carry instead of the operator's login.
func (c *Client) PullRequestValidationToken(ctx context.Context, validationID, nodeID string) (*GitHubToken, error) {
	return c.jobRepoToken(withNodeID(ctx, nodeID), "runsheet/validate", JobSubject{Type: "pull_request", ID: validationID})
}
