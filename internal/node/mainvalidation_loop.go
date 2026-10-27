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
	FailMergeBatchValidation(ctx context.Context, batchID, nodeID, reason string) error
	ClaimWorkItemMainValidation(ctx context.Context, itemID, nodeID, harness string) error
	FailWorkItemMainValidation(ctx context.Context, itemID, reason string) error
	// FinishValidationJob hands the attestation back and ends the claim
	// in one (runsheet ADR 061's finishJob).
	FinishValidationJob(ctx context.Context, subject, id, nodeID string, att gateway.ValidationAttestation) (map[string]any, error)
}

// validationHarness is the harness a main validation's claim names:
// none. A validation runs the repository's suites, no agent, and
// runsheet's validate kind lists harness none (.mesh/jobs/validate.yml);
// claimJob refuses a claim naming the node's agent adapter ("a
// runsheet/validate job runs with none, not claude-code"), which left
// every candidate unclaimed (dl360, batches 85–88, 2026-10-24). Naming
// none takes the kind's own.
const validationHarness = ""

// pullRequestValidationGateway is the platform's half of a hand-opened
// pull request's validation, beside mainValidationGateway: a platform
// before it has no claim or fail, and a test's double may not either.
type pullRequestValidationGateway interface {
	TakesPullRequestValidation(ctx context.Context) bool
	ClaimPullRequestValidation(ctx context.Context, validationID, nodeID, harness string) error
	FailPullRequestValidation(ctx context.Context, validationID, nodeID, reason string) error
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

// pullRequestLabel is a hand-opened pull request's validation's log
// prefix: `pr <repo>#<number>@<sha short>`.
func pullRequestLabel(p gateway.MainValidationPullRequest) string {
	return fmt.Sprintf("pr %s#%d@%s", p.RepoFullName, p.Number, short(p.SHA))
}

// mainJob is one main validation as the loop runs it, a batch
// candidate's or a merged item's: its id (the validating key), log
// prefix and claimed line, the run, and its claim, finish and fail on
// the platform.
type mainJob struct {
	id, label, claimed string
	// kind is what the job validates
	kind   jobKind
	run    func(ctx context.Context) (validate.Attestation, error)
	claim  func(ctx context.Context) error
	finish func(ctx context.Context, att validate.Attestation) error
	fail   func(ctx context.Context, reason string) error
}

func batchJob(l *Loop, gw mainValidationGateway, b gateway.MainValidationBatch, nodeID string) mainJob {
	return mainJob{
		id: b.MergeBatchID, label: batchLabel(b), kind: jobCandidate,
		claimed: fmt.Sprintf("claimed candidate %s@%s", b.CandidateBranch, short(b.SHA)),
		run:     func(ctx context.Context) (validate.Attestation, error) { return l.ValidateBatch(ctx, b) },
		claim: func(ctx context.Context) error {
			return gw.ClaimMergeBatchValidation(ctx, b.MergeBatchID, nodeID, validationHarness)
		},
		finish: func(ctx context.Context, att validate.Attestation) error {
			_, err := gw.FinishValidationJob(ctx, "candidate", b.MergeBatchID, nodeID, gatewayAttestation(att))
			return err
		},
		fail: func(ctx context.Context, reason string) error {
			return gw.FailMergeBatchValidation(ctx, b.MergeBatchID, nodeID, reason)
		},
	}
}

func requestJob(l *Loop, gw mainValidationGateway, r gateway.MainValidationRequest, nodeID string) mainJob {
	return mainJob{
		id: r.WorkItemID, label: mainRequestLabel(r), claimed: "claimed", kind: jobMain,
		run: func(ctx context.Context) (validate.Attestation, error) { return l.ValidateMain(ctx, r) },
		claim: func(ctx context.Context) error {
			return gw.ClaimWorkItemMainValidation(ctx, r.WorkItemID, nodeID, validationHarness)
		},
		finish: func(ctx context.Context, att validate.Attestation) error {
			_, err := gw.FinishValidationJob(ctx, "main", r.WorkItemID, nodeID, gatewayAttestation(att))
			return err
		},
		fail: func(ctx context.Context, reason string) error {
			return gw.FailWorkItemMainValidation(ctx, r.WorkItemID, reason)
		},
	}
}

// pullRequestJob is a hand-opened pull request's validation as the loop
// runs it: claimed and failed by its own commands, finished through
// finishJob on subject pull_request, whose platform half records the
// attestation and finishes the validation itself.
func pullRequestJob(l *Loop, gw mainValidationGateway, pgw pullRequestValidationGateway, p gateway.MainValidationPullRequest, nodeID string) mainJob {
	id := p.PullRequestValidationID
	claimed := "claimed"
	if p.HeadBranch != "" {
		claimed = fmt.Sprintf("claimed head %s (%s)", p.HeadBranch, p.Ref)
	}
	return mainJob{
		id: id, label: pullRequestLabel(p), claimed: claimed, kind: jobPullRequest,
		run: func(ctx context.Context) (validate.Attestation, error) { return l.ValidatePullRequest(ctx, p) },
		claim: func(ctx context.Context) error {
			return pgw.ClaimPullRequestValidation(ctx, id, nodeID, validationHarness)
		},
		finish: func(ctx context.Context, att validate.Attestation) error {
			_, err := gw.FinishValidationJob(ctx, "pull_request", id, nodeID, gatewayAttestation(att))
			return err
		},
		fail: func(ctx context.Context, reason string) error {
			return pgw.FailPullRequestValidation(ctx, id, nodeID, reason)
		},
	}
}

// pullRequestValidation is the gateway's pull request half when the loop
// validates pull requests and the platform serves their claim and fail;
// a platform without them is said once per run, and its pullRequests are
// left alone while batches and requests go on.
func (l *Loop) pullRequestValidation(ctx context.Context, gw mainValidationGateway) (pullRequestValidationGateway, bool) {
	if l.ValidatePullRequest == nil {
		return nil, false
	}
	pgw, ok := gw.(pullRequestValidationGateway)
	if !ok || !pgw.TakesPullRequestValidation(ctx) {
		l.sayValidationOnce("no pull request validation", "main validation: the platform validates no pull request on a node; validating no hand-opened pull request")
		return nil, false
	}
	return pgw, true
}

// Validating is the main validations — batch candidates, merged items
// and hand-opened pull requests — running now.
func (l *Loop) Validating() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.validating)
}

// jobKind is what a main validation validates: a merge batch's
// candidate, a merged item's merge commit on main, or a hand-opened
// pull request's head.
type jobKind int

const (
	jobCandidate jobKind = iota
	jobMain
	jobPullRequest
)

// mainValidation is the gateway's main-validation half when the loop can
// validate batches and the platform serves them; each reason it cannot
// is said once (skipValidation).
func (l *Loop) mainValidation(ctx context.Context) (mainValidationGateway, bool) {
	if l.ValidateBatch == nil && l.ValidateMain == nil && l.ValidatePullRequest == nil {
		l.skipValidation("no validator", "main validation: this node has no validator; validating no merge batch")
		return nil, false
	}
	if !l.takesKind(harness.KindEvaluate) {
		l.skipValidation("takes", "main validation: node.yaml's takes: has no evaluate; validating no merge batch")
		return nil, false
	}
	gw, ok := l.GW.(mainValidationGateway)
	if !ok {
		l.skipValidation("gateway", "main validation: the gateway client (%T) has no main validation; validating no merge batch", l.GW)
		return nil, false
	}
	if !gw.TakesMainValidation(ctx) {
		l.skipValidation("platform", "the platform serves no main validation; validating no merge batch")
		return nil, false
	}
	return gw, true
}

// skipValidation says why the validation path was skipped this tick,
// once per cause: said again only after the path ran or another cause
// was said (a silent skip cost dl360 four batches, 2026-10-24). The
// cause "" marks the path run and says nothing.
func (l *Loop) skipValidation(cause, format string, args ...any) {
	l.mu.Lock()
	said := l.validationSkip == cause
	l.validationSkip = cause
	l.mu.Unlock()
	if !said && cause != "" {
		l.logf(format, args...)
	}
}

// sayValidationOnce logs a note about one job — keyed by why and the
// job's id — the first time it holds; past maxValidationNotes the notes
// kept start over.
func (l *Loop) sayValidationOnce(key, format string, args ...any) {
	l.mu.Lock()
	if l.validationSaid == nil || len(l.validationSaid) >= maxValidationNotes {
		l.validationSaid = map[string]bool{}
	}
	said := l.validationSaid[key]
	l.validationSaid[key] = true
	l.mu.Unlock()
	if !said {
		l.logf(format, args...)
	}
}

// maxValidationNotes bounds sayValidationOnce's memory: a node meets a
// few batches an hour, never hundreds.
const maxValidationNotes = 512

// validateBatches claims and starts a main validation for each served
// batch candidate, then for each served merged item's request, then for
// each served hand-opened pull request, while slots remain, the host is
// not hot and node.yaml's evaluate cap has room, and answers how many
// it started. free is the capacity's free slots. Every batch is offered
// before any request, and every request before any pull request: a pull
// request lands nothing. A skip says its cause once (skipValidation); a job of
// an org the node does not serve, and a claim refused — another node
// holds it, or it waits on none — each say so once per job.
func (l *Loop) validateBatches(ctx context.Context, free int) int {
	free = max(free, 0)
	gw, ok := l.mainValidation(ctx)
	switch {
	case !ok:
		return 0
	case l.diskLow() || l.memLow():
		l.skipValidation("low", "main validation: disk or memory under its floor; validating no merge batch")
		return 0
	case l.hostHot():
		l.skipValidation("hot", "main validation: host hot; validating no merge batch")
		return 0
	case l.holdingValidations():
		l.skipValidation("update", "main validation: a self-update waits for the running validations; validating no new one")
		return 0
	case free <= 0:
		l.skipValidation("full", "main validation: no free slot (capacity %d, %d validating); validating no merge batch", l.Capacity(), l.Validating())
		return 0
	}
	l.skipValidation("", "")
	scopes := l.CurrentScopes()
	served := map[string]bool{}
	for _, sc := range scopes {
		served[sc.ID] = true
	}
	unserved := func(label, id, org string) {
		l.sayValidationOnce("unserved "+id, "%s: org %s is not one this node serves (%s); left for another node", label, orNone(org), orgsOrNone(scopes))
	}
	// every node id's queue read first, so a batch any of them is served
	// comes before every request
	pgw, takesPRs := l.pullRequestValidation(ctx, gw)
	var batches, requests, pulls []mainJob
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
				if seen[b.MergeBatchID] {
					continue
				}
				if !served[b.OrgID] {
					unserved(batchLabel(b), b.MergeBatchID, b.OrgID)
					continue
				}
				seen[b.MergeBatchID] = true
				batches = append(batches, batchJob(l, gw, b, nodeID))
			}
		}
		if l.ValidateMain != nil {
			for _, r := range queue.Requests {
				if seen[r.WorkItemID] {
					continue
				}
				if !served[r.OrgID] {
					unserved(mainRequestLabel(r), r.WorkItemID, r.OrgID)
					continue
				}
				seen[r.WorkItemID] = true
				requests = append(requests, requestJob(l, gw, r, nodeID))
			}
		}
		if takesPRs {
			for _, p := range queue.PullRequests {
				if seen[p.PullRequestValidationID] {
					continue
				}
				if !served[p.OrgID] {
					unserved(pullRequestLabel(p), p.PullRequestValidationID, p.OrgID)
					continue
				}
				seen[p.PullRequestValidationID] = true
				pulls = append(pulls, pullRequestJob(l, gw, pgw, p, nodeID))
			}
		}
	}
	capped := l.takesCap(harness.KindEvaluate)
	started := 0
	for _, j := range append(append(batches, requests...), pulls...) {
		if free <= 0 || (capped.capped && capped.room <= 0) {
			break
		}
		if l.isValidating(j.id) {
			continue // running here already: it takes no new slot
		}
		if l.startMainJob(ctx, j) {
			capped.started()
			started++
			free--
		}
	}
	if waiting := l.waitingCandidates(batches); free <= 0 && waiting != "" {
		l.sayValidationOnce("candidate waits "+waiting, "main validation: a merge batch candidate waits for a free slot (capacity %d, %d validating)", l.Capacity(), l.Validating())
	}
	return started
}

// waitingCandidates is a key naming the candidates served this tick
// that are not validating here, for a once-per-set note.
func (l *Loop) waitingCandidates(batches []mainJob) string {
	var ids []string
	for _, j := range batches {
		if !l.isValidating(j.id) {
			ids = append(ids, j.id)
		}
	}
	return strings.Join(ids, ",")
}

// isValidating is whether the main validation id runs on this node now.
func (l *Loop) isValidating(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.validating[id]
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
		} else {
			// a refusal is the ordinary answer when another node holds
			// it, and rare otherwise: said once per job and reason
			l.sayValidationOnce("refused "+j.id+" "+err.Error(), "%s: claim refused: %v", j.label, err)
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
	if err := j.finish(context.WithoutCancel(ctx), att); err != nil {
		l.logf("%s: finish refused: %v", j.label, err)
		return
	}
	l.logf("%s: attestation recorded %s %s", j.label, short(att.Commit), attestedSuites(att))
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

// mainTokenMinter is the gateway's main validation token, from the
// exchange (*gateway.Client).
type mainTokenMinter interface {
	MainValidationToken(ctx context.Context, itemID, nodeID string) (*gateway.GitHubToken, error)
}

// ValidateMainRequest is validateMainRequest on the merged item's read
// token from the exchange (jobAccess), what the loop's ValidateMain
// runs: a mint refused or failing is the validation's error, never the
// operator's login instead. A gateway that mints none runs on the
// operator's login, as before.
func (r *Runner) ValidateMainRequest(ctx context.Context, req gateway.MainValidationRequest) (validate.Attestation, error) {
	m, ok := r.GW.(mainTokenMinter)
	if !ok {
		return r.validateMainRequest(ctx, req, nil)
	}
	sc, _ := r.Scopes.ByOrg(req.OrgID)
	tok, err := m.MainValidationToken(ctx, req.WorkItemID, sc.NodeID)
	if err != nil {
		err = fmt.Errorf("github token for %s: %w", mainRequestLabel(req), err)
		r.logf("%v; not fetched", err)
		return validate.Attestation{}, err
	}
	return r.validateMainRequest(ctx, req, tok)
}

// pullRequestTokenMinter is the gateway's pull request validation token,
// from the exchange (*gateway.Client).
type pullRequestTokenMinter interface {
	PullRequestValidationToken(ctx context.Context, validationID, nodeID string) (*gateway.GitHubToken, error)
}

// ValidatePullRequest is validatePullRequest on the pull request
// validation's read token from the exchange (jobAccess, subject
// pull_request), what the loop's ValidatePullRequest runs: a mint
// refused or failing is the validation's error, never the operator's
// login instead. A gateway that mints none runs on the operator's login.
func (r *Runner) ValidatePullRequest(ctx context.Context, pr gateway.MainValidationPullRequest) (validate.Attestation, error) {
	m, ok := r.GW.(pullRequestTokenMinter)
	if !ok {
		return r.validatePullRequest(ctx, pr, nil)
	}
	sc, _ := r.Scopes.ByOrg(pr.OrgID)
	tok, err := m.PullRequestValidationToken(ctx, pr.PullRequestValidationID, sc.NodeID)
	if err != nil {
		err = fmt.Errorf("github token for %s: %w", pullRequestLabel(pr), err)
		r.logf("%v; not fetched", err)
		return validate.Attestation{}, err
	}
	return r.validatePullRequest(ctx, pr, tok)
}
