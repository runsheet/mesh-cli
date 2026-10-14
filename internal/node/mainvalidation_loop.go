package node

import (
	"context"
	"errors"
	"fmt"
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
// own — an environment failure, an error, a run past the claim's window
// — fails it with the reason, which releases the claim for another node.
// A merged item's main validation (requests[]) is not taken here.

// MainValidationClaimAfter is how long the platform holds a batch
// validation's claim (platform/aggregates/mergebatch_validation.go): a
// run still going then is stopped and its slot freed.
var MainValidationClaimAfter = 10 * time.Minute

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
}

// errBatchClaimLost and errBatchOverrun are why a running batch
// validation was stopped.
var (
	errBatchClaimLost = errors.New("claim lost")
	errBatchOverrun   = errors.New("ran past the claim's window")
)

// batchLabel is a batch's log prefix: `batch <repo>#<number> attempt <n>`.
func batchLabel(b gateway.MainValidationBatch) string {
	return fmt.Sprintf("batch %s#%d attempt %d", b.RepoFullName, b.Number, b.Attempt)
}

// Validating is the batch validations running now.
func (l *Loop) Validating() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.validating)
}

// mainValidation is the gateway's main-validation half when the loop can
// validate batches and the platform serves them; a platform without the
// fields is said once per run.
func (l *Loop) mainValidation(ctx context.Context) (mainValidationGateway, bool) {
	if l.ValidateBatch == nil || !l.takesKind(harness.KindEvaluate) {
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

// validateBatches claims and starts a batch validation for each served
// batch candidate while free slots remain, the host is not hot and
// node.yaml's evaluate cap has room, and answers how many it started.
// A claim refused — another node holds it, or it waits on none — is
// the ordinary answer and is not logged as an error.
func (l *Loop) validateBatches(ctx context.Context, free int) int {
	gw, ok := l.mainValidation(ctx)
	if !ok || free <= 0 || l.hostHot() {
		return 0
	}
	capped := l.takesCap(harness.KindEvaluate)
	served := map[string]Scope{}
	var nodes []string
	for _, sc := range l.CurrentScopes() {
		served[sc.ID] = sc
	}
	nodes = l.nodeIDs()
	started := 0
	seen := map[string]bool{}
	for _, nodeID := range nodes {
		queue, err := gw.MainValidations(ctx, nodeID)
		if err != nil {
			l.logf("read main validations: %v", err)
			continue
		}
		for _, b := range queue.Batches {
			if seen[b.MergeBatchID] {
				continue
			}
			seen[b.MergeBatchID] = true
			// a batch of an org this node does not serve is not its repo's
			if _, ok := served[b.OrgID]; !ok {
				continue
			}
			if started >= free || (capped.capped && capped.room <= 0) {
				return started
			}
			l.mu.Lock()
			if l.validating[b.MergeBatchID] {
				l.mu.Unlock()
				continue
			}
			if l.validating == nil {
				l.validating = map[string]bool{}
			}
			l.validating[b.MergeBatchID] = true
			l.mu.Unlock()
			if err := gw.ClaimMergeBatchValidation(ctx, b.MergeBatchID, nodeID, l.adapter()); err != nil {
				l.mu.Lock()
				delete(l.validating, b.MergeBatchID)
				l.mu.Unlock()
				if !gateway.IsRefusal(err) {
					l.logf("%s: claim: %v", batchLabel(b), err)
				}
				continue
			}
			l.logf("%s: claimed candidate %s@%s", batchLabel(b), b.CandidateBranch, short(b.SHA))
			capped.started()
			started++
			l.wg.Add(1)
			go func(ctx context.Context, b gateway.MainValidationBatch, nodeID string) {
				defer crash.Guard()
				defer l.wg.Done()
				defer func() {
					l.mu.Lock()
					delete(l.validating, b.MergeBatchID)
					l.mu.Unlock()
					l.notify()
				}()
				l.runBatch(ctx, gw, b, nodeID)
			}(ctx, b, nodeID)
		}
	}
	return started
}

// runBatch validates b's candidate under its claim and ends the claim:
// finished on the candidate's sha whatever the attestation's colour,
// failed with the reason on anything else. A claim the platform
// refuses again (watchBatchClaim) stops the run and sends nothing; a run past
// MainValidationClaimAfter is stopped and failed.
func (l *Loop) runBatch(ctx context.Context, gw mainValidationGateway, b gateway.MainValidationBatch, nodeID string) {
	label := batchLabel(b)
	lost, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	rctx, cancel := context.WithTimeoutCause(lost, MainValidationClaimAfter, errBatchOverrun)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer crash.Guard()
		l.watchBatchClaim(rctx, done, gw, b, nodeID, stop)
	}()

	att, err := l.ValidateBatch(rctx, b)
	if cause := context.Cause(rctx); cause != nil && (errors.Is(cause, errBatchClaimLost) || errors.Is(cause, errBatchOverrun)) {
		if errors.Is(cause, errBatchClaimLost) {
			l.logf("%s: stopped: %v", label, cause)
			return
		}
		reason := fmt.Sprintf("ran past %s, the claim's window; stopped", MainValidationClaimAfter)
		l.logf("%s: stopped: %s", label, reason)
		l.failBatch(context.WithoutCancel(ctx), gw, b, nodeID, reason)
		return
	}
	if err != nil {
		l.failBatch(context.WithoutCancel(ctx), gw, b, nodeID, validate.Reason(err))
		return
	}
	colour := "green"
	if _, red := failedStep(att); red {
		colour = "red"
	}
	l.logf("%s: attested %s (%s)", label, colour, attestedSuites(att))
	if err := gw.FinishMergeBatchValidation(context.WithoutCancel(ctx), b.MergeBatchID, nodeID, att.Commit); err != nil {
		l.logf("%s: finish refused: %v", label, err)
		return
	}
	l.logf("%s: finished", label)
}

// failBatch gives b's claim back with reason and logs it.
func (l *Loop) failBatch(ctx context.Context, gw mainValidationGateway, b gateway.MainValidationBatch, nodeID, reason string) {
	l.logf("%s: failed: %s", batchLabel(b), reason)
	if err := gw.FailMergeBatchValidation(ctx, b.MergeBatchID, nodeID, reason); err != nil {
		l.logf("%s: fail refused: %v", batchLabel(b), err)
	}
}

// watchBatchClaim asks for b's claim again every BatchCheckEvery while
// the run goes on — the claim converges on the same node, so it changes
// nothing while the node holds it — and stops the run, errBatchClaimLost,
// once the platform refuses it: another node holds it, or the batch no
// longer waits on a node. A claim that errs otherwise says nothing.
func (l *Loop) watchBatchClaim(ctx context.Context, done <-chan struct{}, gw mainValidationGateway, b gateway.MainValidationBatch, nodeID string, stop context.CancelCauseFunc) {
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
		if err := gw.ClaimMergeBatchValidation(ctx, b.MergeBatchID, nodeID, l.adapter()); err != nil && gateway.IsRefusal(err) {
			stop(fmt.Errorf("%w: %v", errBatchClaimLost, err))
			return
		}
	}
}

// ValidateCandidate is validateCandidate on the operator's own
// credentials (the platform mints no token for a batch candidate yet):
// what the loop's ValidateBatch runs.
func (r *Runner) ValidateCandidate(ctx context.Context, batch gateway.MainValidationBatch) (validate.Attestation, error) {
	return r.validateCandidate(ctx, batch, nil)
}
