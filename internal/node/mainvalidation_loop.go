package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/crash"
	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/validate"
)

// mainvalidation_loop.go puts a merge batch candidate's validation
// (validateCandidate, runsheet ADR 048 decision 3) on the loop: on the
// evaluation cadence the node reads mainValidations, claims each batch
// of an org it serves while it has a free slot, runs the candidate's
// suites in its own goroutine — counted as a running session against
// the capacity, as a build is — and ends the claim: the attestation is
// the verdict (ADR 054's amendment 2026-10-13), so green and red alike
// are finished on the candidate's sha, and only a failure of the node's
// own — an environment failure, an error, a run past the platform's run limit
// — fails it with the reason, which releases the claim for another node.
// A merged item's main validation (requests[], the main guard asking
// for a run on its merge commit, ADR 054 decision 3) takes the same
// path through the WorkItem twins of the three commands, its merge
// commit attested as the item's (validateMainRequest); a batch is
// served before any request when the slots are short, since it lands
// work already built and attested.

// MainValidationRunAfter is how long the platform lets a claimed batch
// validation run before its timer fails it (runsheet
// platform/aggregates/workitem_mainvalidation.go MainValidationRunAfter,
// mergebatch_validation.go MarkMergeBatchValidationStalled): a run still
// going then is stopped here and its slot freed. The platform's
// ten-minute MainValidationClaimAfter is how long a *requested*
// validation waits for a node to claim it, which a run never needs —
// mirroring that one here stopped runsheet's own twelve-minute suites
// at ten (dl360, 2026-10-14: batches 33, 35, 36 abandoned).
var MainValidationRunAfter = 60 * time.Minute

// DefaultBatchCheckEvery is how often a running batch validation reads
// mainValidations back to see its claim is still the node's.
const DefaultBatchCheckEvery = time.Minute

// mainValidationGateway is the platform's half of a batch validation:
// the gateway client has it, a test's double may not.
type mainValidationGateway interface {
	TakesMainValidation(ctx context.Context) bool
	MainValidations(ctx context.Context, nodeID string) (gateway.MainValidationQueue, error)
	ClaimMergeBatchValidation(ctx context.Context, batchID, nodeID, harness string) error
	FinishMergeBatchValidation(ctx context.Context, batchID, nodeID, sha string) error
	FailMergeBatchValidation(ctx context.Context, batchID, nodeID, reason string) error
	ClaimWorkItemMainValidation(ctx context.Context, itemID, nodeID, harness string) error
	FinishWorkItemMainValidation(ctx context.Context, itemID, sha string) error
	FailWorkItemMainValidation(ctx context.Context, itemID, reason string) error
}

// errBatchClaimLost and errBatchOverrun are why a running batch
// validation was stopped.
var (
	errBatchClaimLost = errors.New("claim lost")
	errBatchOverrun   = errors.New("ran past the platform's run limit")
)

// batchLabel is a batch's log prefix: `batch <repo>#<number> attempt <n>`.
func batchLabel(b gateway.MainValidationBatch) string {
	return fmt.Sprintf("batch %s#%d attempt %d", b.RepoFullName, b.Number, b.Attempt)
}

// mainRequestLabel is a merged item's main validation's log prefix:
// `main <repo>@<sha short> item <seq>`.
func mainRequestLabel(r gateway.MainValidationRequest) string {
	return fmt.Sprintf("main %s@%s item %d", r.RepoFullName, short(r.SHA), r.Seq)
}

// mainJob is one main validation as the loop runs it, a batch
// candidate's or a merged item's: its id (the validating key), log
// prefix and claimed line, the run, and its claim, finish and fail on
// the platform.
type mainJob struct {
	id, label, claimed string
	run                func(ctx context.Context) (validate.Attestation, error)
	claim              func(ctx context.Context) error
	finish             func(ctx context.Context, sha string) error
	fail               func(ctx context.Context, reason string) error
}

func batchJob(l *Loop, gw mainValidationGateway, b gateway.MainValidationBatch, nodeID string) mainJob {
	return mainJob{
		id: b.MergeBatchID, label: batchLabel(b),
		claimed: fmt.Sprintf("claimed candidate %s@%s", b.CandidateBranch, short(b.SHA)),
		run:     func(ctx context.Context) (validate.Attestation, error) { return l.ValidateBatch(ctx, b) },
		claim: func(ctx context.Context) error {
			return gw.ClaimMergeBatchValidation(ctx, b.MergeBatchID, nodeID, l.adapter())
		},
		finish: func(ctx context.Context, sha string) error {
			return gw.FinishMergeBatchValidation(ctx, b.MergeBatchID, nodeID, sha)
		},
		fail: func(ctx context.Context, reason string) error {
			return gw.FailMergeBatchValidation(ctx, b.MergeBatchID, nodeID, reason)
		},
	}
}

func requestJob(l *Loop, gw mainValidationGateway, r gateway.MainValidationRequest, nodeID string) mainJob {
	return mainJob{
		id: r.WorkItemID, label: mainRequestLabel(r), claimed: "claimed",
		run: func(ctx context.Context) (validate.Attestation, error) { return l.ValidateMain(ctx, r) },
		claim: func(ctx context.Context) error {
			return gw.ClaimWorkItemMainValidation(ctx, r.WorkItemID, nodeID, l.adapter())
		},
		finish: func(ctx context.Context, sha string) error {
			return gw.FinishWorkItemMainValidation(ctx, r.WorkItemID, sha)
		},
		fail: func(ctx context.Context, reason string) error {
			return gw.FailWorkItemMainValidation(ctx, r.WorkItemID, reason)
		},
	}
}

// Validating is the main validations — batch candidates and merged
// items — running now.
func (l *Loop) Validating() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.validating)
}

// mainValidation is the gateway's main-validation half when the loop can
// validate batches and the platform serves them; a platform without the
// fields is said once per run.
func (l *Loop) mainValidation(ctx context.Context) (mainValidationGateway, bool) {
	if (l.ValidateBatch == nil && l.ValidateMain == nil) || !l.takesKind(harness.KindEvaluate) {
		return nil, false
	}
	gw, ok := l.GW.(mainValidationGateway)
	if !ok || !gw.TakesMainValidation(ctx) {
		l.mu.Lock()
		said := l.noMainValidationSaid
		l.noMainValidationSaid = true
		l.mu.Unlock()
		if !said {
			l.logf("the platform serves no main validation; validating no merge batch")
		}
		return nil, false
	}
	return gw, true
}

// validateBatches claims and starts a main validation for each served
// batch candidate, then for each served merged item's request, while
// free slots remain, the host is not hot and node.yaml's evaluate cap
// has room, and answers how many it started. Every batch is offered
// before any request. A claim refused — another node holds it, or it
// waits on none — is the ordinary answer and is not logged as an error.
func (l *Loop) validateBatches(ctx context.Context, free int) int {
	gw, ok := l.mainValidation(ctx)
	if !ok || free <= 0 || l.hostHot() {
		return 0
	}
	served := map[string]bool{}
	for _, sc := range l.CurrentScopes() {
		served[sc.ID] = true
	}
	// every node id's queue read first, so a batch any of them is served
	// comes before every request
	var batches, requests []mainJob
	seen := map[string]bool{}
	for _, nodeID := range l.nodeIDs() {
		queue, err := gw.MainValidations(ctx, nodeID)
		if err != nil {
			l.logf("read main validations: %v", err)
			continue
		}
		if l.ValidateBatch != nil {
			for _, b := range queue.Batches {
				// a batch of an org this node does not serve is not its repo's
				if seen[b.MergeBatchID] || !served[b.OrgID] {
					continue
				}
				seen[b.MergeBatchID] = true
				batches = append(batches, batchJob(l, gw, b, nodeID))
			}
		}
		if l.ValidateMain != nil {
			for _, r := range queue.Requests {
				if seen[r.WorkItemID] || !served[r.OrgID] {
					continue
				}
				seen[r.WorkItemID] = true
				requests = append(requests, requestJob(l, gw, r, nodeID))
			}
		}
	}
	capped := l.takesCap(harness.KindEvaluate)
	started := 0
	for _, j := range append(batches, requests...) {
		if started >= free || (capped.capped && capped.room <= 0) {
			break
		}
		if l.startMainJob(ctx, j) {
			capped.started()
			started++
		}
	}
	return started
}

// startMainJob claims j and, granted, runs it in its own goroutine,
// marked validating — a session against the capacity — until it ends.
func (l *Loop) startMainJob(ctx context.Context, j mainJob) bool {
	l.mu.Lock()
	if l.validating[j.id] {
		l.mu.Unlock()
		return false
	}
	if l.validating == nil {
		l.validating = map[string]bool{}
	}
	l.validating[j.id] = true
	l.mu.Unlock()
	if err := j.claim(ctx); err != nil {
		l.mu.Lock()
		delete(l.validating, j.id)
		l.mu.Unlock()
		if !gateway.IsRefusal(err) {
			l.logf("%s: claim: %v", j.label, err)
		}
		return false
	}
	l.logf("%s: %s", j.label, j.claimed)
	l.wg.Add(1)
	go func() {
		defer crash.Guard()
		defer l.wg.Done()
		defer func() {
			l.mu.Lock()
			delete(l.validating, j.id)
			l.mu.Unlock()
			l.notify()
		}()
		l.runMainJob(ctx, j)
	}()
	return true
}

// runMainJob validates j under its claim and ends the claim: finished
// on the attested sha whatever the attestation's colour, failed with
// the reason on anything else. A claim the platform refuses again
// (watchMainClaim) stops the run and sends nothing; a run past
// MainValidationRunAfter is stopped and failed.
func (l *Loop) runMainJob(ctx context.Context, j mainJob) {
	lost, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	rctx, cancel := context.WithTimeoutCause(lost, MainValidationRunAfter, errBatchOverrun)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer crash.Guard()
		l.watchMainClaim(rctx, done, j, stop)
	}()

	att, err := j.run(rctx)
	if cause := context.Cause(rctx); cause != nil && (errors.Is(cause, errBatchClaimLost) || errors.Is(cause, errBatchOverrun)) {
		if errors.Is(cause, errBatchClaimLost) {
			l.logf("%s: stopped: %v", j.label, cause)
			return
		}
		reason := fmt.Sprintf("ran past %s, the platform's run limit; stopped", MainValidationRunAfter)
		l.logf("%s: stopped: %s", j.label, reason)
		l.failMainJob(context.WithoutCancel(ctx), j, reason)
		return
	}
	if err != nil {
		l.failMainJob(context.WithoutCancel(ctx), j, validate.Reason(err))
		return
	}
	colour := "green"
	if _, red := failedStep(att); red {
		colour = "red"
	}
	l.logf("%s: attested %s (%s)", j.label, colour, attestedSuites(att))
	if err := j.finish(context.WithoutCancel(ctx), att.Commit); err != nil {
		l.logf("%s: finish refused: %v", j.label, err)
		return
	}
	l.logf("%s: finished", j.label)
}

// failMainJob gives j's claim back with reason and logs it.
func (l *Loop) failMainJob(ctx context.Context, j mainJob, reason string) {
	l.logf("%s: failed: %s", j.label, reason)
	if err := j.fail(ctx, reason); err != nil {
		l.logf("%s: fail refused: %v", j.label, err)
	}
}

// watchMainClaim asks for j's claim again every BatchCheckEvery while
// the run goes on — the claim converges on the same node, so it changes
// nothing while the node holds it — and stops the run, errBatchClaimLost,
// once the platform refuses it: another node holds it, or the
// validation no longer waits on a node. A claim that errs otherwise
// says nothing.
func (l *Loop) watchMainClaim(ctx context.Context, done <-chan struct{}, j mainJob, stop context.CancelCauseFunc) {
	every := l.BatchCheckEvery
	if every <= 0 {
		every = DefaultBatchCheckEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
		}
		if err := j.claim(ctx); err != nil && gateway.IsRefusal(err) {
			stop(fmt.Errorf("%w: %v", errBatchClaimLost, err))
			return
		}
	}
}

// batchTokenMinter is the gateway's batch form of issueItemRepoToken
// (IssueMergeBatchRepoToken): the client has it, a test's double may not.
type batchTokenMinter interface {
	IssueMergeBatchRepoToken(ctx context.Context, batch gateway.MainValidationBatch, nodeID string) (*gateway.GitHubToken, error)
}

// ValidateCandidate is validateCandidate on the batch's own token
// (NewBatchCred): what the loop's ValidateBatch runs. A platform that
// mints none falls back to the operator's login, said once per run; a
// mint refused or failing is the validation's error, never the
// operator's login instead.
func (r *Runner) ValidateCandidate(ctx context.Context, batch gateway.MainValidationBatch) (validate.Attestation, error) {
	cred, err := NewBatchCred(ctx, r.GW, r.Scopes, batch, r.BatchFallback, r.logf)
	if err != nil {
		r.logf("%s: %v; not fetched", candidatePrefix(batch), err)
		return validate.Attestation{}, err
	}
	return r.validateCandidate(ctx, batch, cred)
}

// NewBatchCred mints batch's candidate token (IssueMergeBatchRepoToken)
// for the node holding its validation claim, renewed by the same form
// when near expiry (ItemCred.ForFetch, before every fetch). It writes
// nothing into a checkout: the candidate's fetches carry it by
// OnItemToken. A platform without the batch form (gateway.IsUnsupported,
// or a gateway that has none) answers nil, nil, the operator's login,
// noted once through fb; any other error is `github token for merge
// batch N: …`.
func NewBatchCred(ctx context.Context, gw Gateway, scopes Scopes, batch gateway.MainValidationBatch, fb *TokenFallback, logf func(string, ...any)) (*ItemCred, error) {
	prefix := candidatePrefix(batch)
	m, ok := gw.(batchTokenMinter)
	if !ok {
		fb.NoteBatch(prefix, logf)
		return nil, nil
	}
	sc, _ := scopes.ByOrg(batch.OrgID)
	// every mint, the first and each renewal, is checked before any
	// fetch can carry it: a token withGitToken would not put on git
	// would leave the fetch on the operator's login, and one for
	// another repo is not the batch's
	issue := func(ctx context.Context) (*gateway.GitHubToken, error) {
		tok, err := m.IssueMergeBatchRepoToken(ctx, batch, sc.NodeID)
		if err != nil {
			return nil, err
		}
		return tok, checkBatchToken(batch, tok)
	}
	tok, err := issue(ctx)
	if gateway.IsUnsupported(err) {
		fb.NoteBatch(prefix, logf)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("github token for merge batch %d: %w", batch.Number, err)
	}
	return &ItemCred{gw: gw, itemID: batch.MergeBatchID, nodeID: sc.NodeID, issue: issue, tok: tok}, nil
}

// checkBatchToken refuses a minted token the candidate's fetch cannot go
// out on alone: none, one not shaped like a token (tokenShape, which
// withGitToken would silently leave off the fetch), or one issued for
// another repo than the batch's.
func checkBatchToken(batch gateway.MainValidationBatch, tok *gateway.GitHubToken) error {
	if tok == nil || tok.Token == "" {
		return errors.New("the platform minted no token")
	}
	if !tokenShape.MatchString(tok.Token) {
		return errors.New("the platform minted a malformed token")
	}
	if tok.Repository != "" && !strings.EqualFold(tok.Repository, batch.RepoFullName) {
		return fmt.Errorf("issued for %s, the batch is on %s", tok.Repository, batch.RepoFullName)
	}
	return nil
}

// ValidateMainRequest is validateMainRequest on the operator's own
// credentials, as ValidateCandidate: what the loop's ValidateMain runs.
func (r *Runner) ValidateMainRequest(ctx context.Context, req gateway.MainValidationRequest) (validate.Attestation, error) {
	return r.validateMainRequest(ctx, req, nil)
}
