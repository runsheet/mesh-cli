package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
	"projectmesh.io/mesh-cli/internal/validate"
)

// mainvalidation.go is a merge batch candidate's validation (runsheet
// ADR 048 decision 3 and ADR 054, both amended 2026-10-13): main plus
// the queued attested pull requests, merged clean by the integrator onto
// a candidate branch, is proved by a node running the repository's
// .runsheet/validate.yml on exactly the candidate's sha and attesting it
// — green fast-forwards main to it. There is no build session: the
// candidate is fetched on the repo's token into a fresh worktree of the
// shared clone, the suites its diff from the default branch selects run
// in the sandbox pod with its services as a build's validation does
// (validator.start, validator.run), and the attestation goes up with
// the batch's id (RecordBatchCandidateAttestation). The worktree goes
// after.

// CandidateKind is the sandbox session kind of a candidate's validation.
const CandidateKind harness.Kind = "validate"

// CandidateDir is where batch's candidate is checked out under the work
// root: <root>/merge-batches/<mergeBatchId>-<attempt>, a worktree of the
// repo's shared clone beside the items' (its grandparent the work root,
// as an item's <root>/<goal>/<seq> has, for the locks and the sweep).
func CandidateDir(root string, batch gateway.MainValidationBatch) string {
	return filepath.Join(root, "merge-batches", fmt.Sprintf("%s-%d", batch.MergeBatchID, batch.Attempt))
}

// CandidateHeadError is a candidate branch whose fetched head is not the
// batch's sha: the integrator moved it, or the batch is stale. Nothing
// is checked out, run or attested.
type CandidateHeadError struct {
	Branch, Want, Got string
}

func (e *CandidateHeadError) Error() string {
	return fmt.Sprintf("candidate %s is at %s, not the batch's %s; not validated", e.Branch, short(e.Got), short(e.Want))
}

// CommitNotOnBranchError is a merged item's merge commit the default
// branch, as fetched, does not hold: a force-push dropped it, or the
// request is stale. Nothing is checked out, run or attested.
type CommitNotOnBranchError struct {
	Branch, SHA string
}

func (e *CommitNotOnBranchError) Error() string {
	return fmt.Sprintf("merge commit %s is not on %s; not validated", short(e.SHA), e.Branch)
}

// PullRequestHeadError is a pull request whose fetched head is not the
// validation's sha: a person pushed since the platform asked. Nothing is
// checked out, run or attested; the platform asks again for the new head.
type PullRequestHeadError struct {
	Ref, Want, Got string
}

func (e *PullRequestHeadError) Error() string {
	return fmt.Sprintf("%s is at %s, not the validation's %s; not validated", e.Ref, short(e.Got), short(e.Want))
}

// pullRequestWorktrees checks a pull request's head out (*Git).
type pullRequestWorktrees interface {
	AddPullRequestWorktree(ctx context.Context, repo, dir, ref, sha string) error
}

// candidateWorktrees makes and removes a candidate's checkout (*Git).
type candidateWorktrees interface {
	AddCandidateWorktree(ctx context.Context, repo, dir, branch, sha string) error
	AddCommitWorktree(ctx context.Context, repo, dir, branch, sha string) error
	RemoveWorkspace(ctx context.Context, dir string) error
}

// ErrNoCandidateWorktrees is a Runner whose Workspacer cannot make a
// candidate's worktree.
var ErrNoCandidateWorktrees = errors.New("the node's workspace makes no candidate worktrees")

// validateCandidate validates batch's candidate and uploads its
// attestation: the candidate branch fetched on cred (the batch's minted
// token, renewed near expiry; nil is the operator's own credentials,
// the fallback) into a fresh
// worktree of the shared clone, refused as a *CandidateHeadError when
// its head is not batch.SHA; the suites the diff from the default branch
// selects (validate.Select, plus every `always` one) run in the sandbox
// pod with the checkout's services, else on the host; the attestation's
// commit batch.SHA, uploaded with mergeBatchId. A red suite is a red
// attestation, uploaded, and nil: the attestation is the verdict. A
// failure of the node's own — a step whose environment kind is set
// (validate.EnvironmentKind), a fetch GitHub refused, a required service
// that did not come up — is returned (validate.Kinded reads its kind)
// with nothing attested, so the caller fails the batch's validation and
// never calls it red. The worktree is removed on every path.
func (r *Runner) validateCandidate(ctx context.Context, batch gateway.MainValidationBatch, cred *ItemCred) (validate.Attestation, error) {
	label := fmt.Sprintf("batch-%d", batch.Number)
	prefix := candidatePrefix(batch)
	dir, err := batchDirFor(r.Layout, r.Scopes, batch.OrgID, batch.MergeBatchID, "candidate", int(batch.Attempt), prefix,
		func(root string) string { return CandidateDir(root, batch) }, r.WorkRoot)
	if err != nil {
		return validate.Attestation{}, fmt.Errorf("%s: %w", prefix, err)
	}
	t := validationTarget{
		item:      gateway.WorkItem{OrgID: batch.OrgID, RepoID: batch.RepoID},
		dir:       dir,
		base:      batch.DefaultBranch,
		prefix:    prefix,
		label:     label,
		session:   func() string { return "" },
		redact:    func(err error) error { return err },
		report:    func(_ context.Context, note string) { r.logf("%s: %s", prefix, note) },
		batchID:   batch.MergeBatchID,
		cred:      cred,
		tokenName: fmt.Sprintf("merge batch %d", batch.Number),
	}
	return r.validateCommit(ctx, t, mainCommit{repo: batch.RepoFullName, branch: batch.CandidateBranch, sha: batch.SHA, atHead: true,
		what: "candidate " + batch.CandidateBranch, token: fmt.Sprintf("merge batch %d", batch.Number)})
}

// candidatePrefix is a candidate's validation's log prefix: `merge batch
// <number> (<owner/name>)`.
func candidatePrefix(batch gateway.MainValidationBatch) string {
	return fmt.Sprintf("merge batch %d (%s)", batch.Number, batch.RepoFullName)
}

// MainRequestDir is where a merged item's main validation checks its
// merge commit out under the work root:
// <root>/main-validations/<workItemId>-<attempt>, beside the batches'.
func MainRequestDir(root string, req gateway.MainValidationRequest) string {
	return filepath.Join(root, "main-validations", fmt.Sprintf("%s-%d", req.WorkItemID, req.Attempt))
}

// validateMainRequest validates a merged item's merge commit on the
// default branch (runsheet ADR 054 decision 3, the main guard's run on
// a node) as validateCandidate validates a candidate: the default
// branch fetched on tok, req.SHA refused as a *CommitNotOnBranchError
// when the branch does not hold it, and checked out detached; the
// suites the merge's own diff selects (from its first parent, the
// default branch before the merge) run; the attestation the item's,
// RecordValidationAttestation with no mergeBatchId.
func (r *Runner) validateMainRequest(ctx context.Context, req gateway.MainValidationRequest, tok *gateway.GitHubToken) (validate.Attestation, error) {
	label := fmt.Sprintf("main-%d", req.Seq)
	prefix := mainRequestLabel(req)
	dir, err := batchDirFor(r.Layout, r.Scopes, req.OrgID, mainJobID(req.WorkItemID), "main", int(req.Attempt), prefix,
		func(root string) string { return MainRequestDir(root, req) }, r.WorkRoot)
	if err != nil {
		return validate.Attestation{}, fmt.Errorf("%s: %w", prefix, err)
	}
	t := validationTarget{
		item:     gateway.WorkItem{ID: req.WorkItemID, GoalID: req.GoalID, Seq: int(req.Seq), OrgID: req.OrgID, RepoID: req.RepoID},
		dir:      dir,
		base:     req.DefaultBranch,
		diffFrom: req.SHA + "^1",
		prefix:   prefix,
		label:    label,
		session:  func() string { return "" },
		redact:   func(err error) error { return err },
		report:   func(_ context.Context, note string) { r.logf("%s: %s", prefix, note) },
		repoTok:  tok,
	}
	return r.validateCommit(ctx, t, mainCommit{repo: req.RepoFullName, branch: req.DefaultBranch, sha: req.SHA,
		what: "merge commit on " + req.DefaultBranch, token: fmt.Sprintf("item %d", req.Seq)})
}

// PullRequestDir is where a hand-opened pull request's validation checks
// its head out under the work root:
// <root>/pull-requests/<pullRequestValidationId>, beside the batches'.
func PullRequestDir(root string, pr gateway.MainValidationPullRequest) string {
	return filepath.Join(root, "pull-requests", pr.PullRequestValidationID)
}

// validatePullRequest validates the head of a pull request a person
// opened by hand as validateMainRequest validates a merge commit: the
// pull request's ref (refs/pull/<n>/head, on the base repository, so a
// head on a fork is fetched the same way) fetched on tok, refused as a
// *PullRequestHeadError when its head is not pr.SHA, and checked out
// detached; every suite validate.yml declares runs (allSuites), since
// nothing selected them for this head; the attestation is pr.SHA's with
// its tree, handed back by the loop through finishJob.
func (r *Runner) validatePullRequest(ctx context.Context, pr gateway.MainValidationPullRequest, tok *gateway.GitHubToken) (validate.Attestation, error) {
	label := fmt.Sprintf("pr-%d", pr.Number)
	prefix := pullRequestLabel(pr)
	dir, err := batchDirFor(r.Layout, r.Scopes, pr.OrgID, pr.PullRequestValidationID, "pr", 1, prefix,
		func(root string) string { return PullRequestDir(root, pr) }, r.WorkRoot)
	if err != nil {
		return validate.Attestation{}, fmt.Errorf("%s: %w", prefix, err)
	}
	ref := pr.Ref
	if ref == "" {
		ref = fmt.Sprintf("refs/pull/%d/head", pr.Number)
	}
	t := validationTarget{
		item:      gateway.WorkItem{OrgID: pr.OrgID, RepoID: pr.RepoID},
		dir:       dir,
		base:      pr.DefaultBranch,
		prefix:    prefix,
		label:     label,
		session:   func() string { return "" },
		redact:    func(err error) error { return err },
		report:    func(_ context.Context, note string) { r.logf("%s: %s", prefix, note) },
		batchID:   pr.PullRequestValidationID,
		repoTok:   tok,
		allSuites: true,
	}
	return r.validateCommit(ctx, t, mainCommit{repo: pr.RepoFullName, branch: ref, sha: pr.SHA, pull: true,
		what: fmt.Sprintf("pull request #%d head", pr.Number), token: fmt.Sprintf("pull request #%d", pr.Number)})
}

// orUnnamed is s, else `unnamed`: a token whose repo or access the
// platform did not say.
func orUnnamed(s string) string {
	if s == "" {
		return "unnamed"
	}
	return s
}

// mainCommit is the commit a main validation checks out: sha on
// repo's branch — its head when atHead (a candidate), else any commit
// the branch holds (a merge commit main has moved past) — what the log
// calls it, and whose token fetches it (`github token for <token>: …`).
//
// pull, set, is a hand-opened pull request's head: branch is then its
// ref (refs/pull/<n>/head), fetched as it is, whose head must be sha.
type mainCommit struct {
	repo, branch, sha string
	atHead, pull      bool
	what, token       string
}

// validateCommit is a main validation's body, a batch candidate's or a
// merged item's: c materialised from GitHub's served diff in node-owned
// scratch (checkoutServed) — else, when the served diff cannot give it,
// checked out at t.dir as a worktree of the shared clone — the suites
// run in the sandbox pod with the checkout's services, else on the
// host, and the attestation uploaded for t — by mergeBatchId when
// t.batchID is set, else as t.item's. A red suite is a red attestation
// and nil; a failure of the node's own is returned with nothing
// attested. The scratch or worktree is removed on every path.
// A pod the container runtime itself cannot start (a
// *sandbox.RuntimeError) is logged once naming podman's words and told
// to RuntimeBroken; the loop's fail, its reason `runtime_broken: …`
// (validate.Reason), releases the claim back to requested (ADR-030).
func (r *Runner) validateCommit(ctx context.Context, t validationTarget, c mainCommit) (validate.Attestation, error) {
	att, err := r.validateCommitIn(ctx, t, c)
	if re := RuntimeErrorOf(err); re != nil && ctx.Err() == nil {
		r.logf("%s: container runtime broken, claim released: %s", t.prefix, oneLine(RuntimeReason(re)))
		if r.RuntimeBroken != nil {
			r.RuntimeBroken(re)
		}
	}
	return att, err
}

func (r *Runner) validateCommitIn(ctx context.Context, t validationTarget, c mainCommit) (validate.Attestation, error) {
	prefix := t.prefix
	r.logf("%s: validating %s at %s", prefix, c.what, short(c.sha))
	dir, served, err := r.checkoutServed(ctx, t, c)
	if err == nil {
		t.dir, t.served = dir, served
		defer func() {
			// the validation's close removed it after the pod; this is
			// every path that ended before one (validator.removeScratch)
			if err := r.validator().removeScratch(dir); err != nil {
				r.logf("%s: remove scratch %s: %v", prefix, dir, err)
			}
			_ = os.RemoveAll(sessionunit.Dir(dir))
			_ = os.Remove(dir + ".attestation.json")
		}()
		if c.atHead {
			if tok := t.cred.Token(); tok != nil {
				r.logf("%s: candidate read on minted token (%s, %s)", prefix, orUnnamed(tok.Repository), orUnnamed(tok.Access))
			}
		}
	} else {
		fb, ok := fallbackOf(err)
		if !ok {
			var he *CandidateHeadError
			var ne *CommitNotOnBranchError
			var pe *PullRequestHeadError
			var te *fetchTokenError
			if errors.As(err, &he) || errors.As(err, &ne) || errors.As(err, &pe) {
				r.logf("%s: %v", prefix, err)
			} else if errors.As(err, &te) {
				r.logf("%s: %v; not fetched", prefix, err)
			}
			return validate.Attestation{}, err
		}
		r.noteServedFallback(prefix, fb)
		cleanup, err := r.checkoutWorktree(ctx, t, c)
		defer cleanup()
		if err != nil {
			return validate.Attestation{}, err
		}
	}
	return r.validateCheckout(ctx, t, c)
}

// checkoutWorktree checks c out at t.dir, a detached worktree of the
// shared clone fetched on t's token; its cleanup, never nil, removes it.
func (r *Runner) checkoutWorktree(ctx context.Context, t validationTarget, c mainCommit) (func(), error) {
	wt, ok := r.WS.(candidateWorktrees)
	if !ok {
		return func() {}, ErrNoCandidateWorktrees
	}
	dir, prefix := t.dir, t.prefix
	vr := r.validator()
	cleanup := func() {
		if err := wt.RemoveWorkspace(context.WithoutCancel(ctx), dir); err != nil {
			r.logf("%s: remove %s: %v", prefix, dir, err)
		}
		_ = os.RemoveAll(sessionunit.Dir(dir))
		_ = os.Remove(dir + ".attestation.json")
	}
	add := wt.AddCommitWorktree
	if c.atHead {
		add = wt.AddCandidateWorktree
	}
	if c.pull {
		pw, ok := r.WS.(pullRequestWorktrees)
		if !ok {
			return cleanup, ErrNoCandidateWorktrees
		}
		add = pw.AddPullRequestWorktree
	}
	fctx, err := vr.onFreshItemToken(ctx, t)
	if err != nil {
		err = &fetchTokenError{name: c.token, err: err}
		r.logf("%s: %v; not fetched", prefix, err)
		return cleanup, err
	}
	if err := add(fctx, c.repo, dir, c.branch, c.sha); err != nil {
		var he *CandidateHeadError
		var ne *CommitNotOnBranchError
		var pe *PullRequestHeadError
		if errors.As(err, &he) || errors.As(err, &ne) || errors.As(err, &pe) {
			r.logf("%s: %v", prefix, err)
			return cleanup, err
		}
		return cleanup, validate.Classify(fmt.Errorf("fetch %s: %w", c.branch, err))
	}
	if c.atHead {
		if tok := t.cred.Token(); tok != nil {
			r.logf("%s: candidate fetched on minted token (%s, %s)", prefix, orUnnamed(tok.Repository), orUnnamed(tok.Access))
		} else if t.repoTok == nil {
			r.logf("%s: candidate fetched on the operator's own login", prefix)
		}
	}
	return cleanup, nil
}

// validateCheckout runs and attests c's validation at t.dir, its
// checkout made.
func (r *Runner) validateCheckout(ctx context.Context, t validationTarget, c mainCommit) (validate.Attestation, error) {
	dir, prefix := t.dir, t.prefix
	vr := r.validator()
	if _, err := validate.Load(dir); err != nil {
		// no validate.yml on the commit is nothing to attest: the caller's
		return validate.Attestation{}, err
	}
	pod, err := openOrgSessionPod(ctx, r.Sandbox, r.Layout, r.Scopes, t.item.OrgID, dir, t.label, CandidateKind, sandbox.GitIdentity{}, r.logf)
	defer pod.Close()
	if err != nil {
		return validate.Attestation{}, err
	}
	if _, failed := pod.RequiredFailure(); failed {
		return validate.Attestation{}, errors.New(pod.ReleaseReason())
	}
	if pod != nil {
		t.pod = pod.Session
	}
	v, err := vr.start(ctx, t)
	if err != nil {
		return validate.Attestation{}, err
	}
	defer v.close()
	att, err := vr.run(ctx, t, v)
	if err != nil {
		return att, err
	}
	if att.Commit != c.sha {
		return att, fmt.Errorf("validated %s, not the %s %s", short(att.Commit), c.what, short(c.sha))
	}
	if failed, ok := failedStep(att); ok && failed.EnvironmentKind != "" {
		vf := &validationFailure{Step: failed}
		r.logf("%s: %s; not attested", prefix, validate.Reason(vf))
		return att, vf
	}
	// the attestation is the job's finish (FinishValidationJob, runsheet
	// ADR 061): kept beside the checkout here, handed back with the end
	// of the claim
	if err := writeAttestationAt(t.dir+".attestation.json", att); err != nil {
		r.logf("%s: validation: %v", prefix, err)
	}
	return att, nil
}

// AddCandidateWorktree checks a merge batch candidate out at dir: the
// repo's shared clone made current (syncSharedClone), branch fetched
// into refs/remotes/origin/<branch> under the per-repo fetch lock, its
// head compared with sha — another is a *CandidateHeadError and nothing
// is added — and a detached worktree of sha added at dir under the
// repo's lock. A checkout a crashed run left at dir is removed first.
// Its fetches go out on whatever token ctx carries (OnItemToken).
func (g *Git) AddCandidateWorktree(ctx context.Context, repo, dir, branch, sha string) error {
	return g.addDetachedWorktree(ctx, repo, dir, branch, sha, func(clone, ref string) error {
		head, err := g.run(ctx, clone, "git", "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return err
		}
		if head != sha {
			return &CandidateHeadError{Branch: branch, Want: sha, Got: head}
		}
		return nil
	})
}

// AddCommitWorktree checks a merged item's merge commit out at dir as
// AddCandidateWorktree checks a candidate out, but the fetched branch
// need only hold sha, not end on it: main moves on past a merge. A
// branch that does not hold it is a *CommitNotOnBranchError.
func (g *Git) AddCommitWorktree(ctx context.Context, repo, dir, branch, sha string) error {
	return g.addDetachedWorktree(ctx, repo, dir, branch, sha, func(clone, ref string) error {
		if _, err := g.run(ctx, clone, "git", "merge-base", "--is-ancestor", sha, ref); err != nil {
			return &CommitNotOnBranchError{Branch: branch, SHA: sha}
		}
		return nil
	})
}

// AddPullRequestWorktree checks a pull request's head out at dir as
// AddCandidateWorktree checks a candidate out, but ref is a full ref of
// the base repository (refs/pull/<n>/head), fetched into
// refs/remotes/origin/pull/<n>/head, so a head on a fork comes the same
// way. A head other than sha is a *PullRequestHeadError.
func (g *Git) AddPullRequestWorktree(ctx context.Context, repo, dir, ref, sha string) error {
	return g.addDetachedWorktree(ctx, repo, dir, ref, sha, func(clone, local string) error {
		head, err := g.run(ctx, clone, "git", "rev-parse", "--verify", local+"^{commit}")
		if err != nil {
			return err
		}
		if head != sha {
			return &PullRequestHeadError{Ref: ref, Want: sha, Got: head}
		}
		return nil
	})
}

// fetchRefspec is the refspec addDetachedWorktree fetches branch by, and
// the local ref it lands in: a branch name is refs/heads/<branch> into
// refs/remotes/origin/<branch>; a full ref (refs/pull/<n>/head) is
// fetched as it is into refs/remotes/origin/<ref less refs/>.
func fetchRefspec(branch string) (spec, local string) {
	if rest, ok := strings.CutPrefix(branch, "refs/"); ok {
		local = "refs/remotes/origin/" + rest
		return "+" + branch + ":" + local, local
	}
	local = "refs/remotes/origin/" + branch
	return "+refs/heads/" + branch + ":" + local, local
}

// addDetachedWorktree fetches branch into the shared clone, asks check
// of it, and adds a detached worktree of sha at dir. A dir in a job is a
// clone of its own instead (addDetachedClone, ADR-039).
func (g *Git) addDetachedWorktree(ctx context.Context, repo, dir, branch, sha string, check func(clone, ref string) error) error {
	if _, err := os.Stat(dir); err == nil {
		if err := g.RemoveWorkspace(ctx, dir); err != nil {
			return err
		}
	}
	if _, _, ok := jobOf(dir); ok {
		return g.addDetachedClone(ctx, repo, dir, branch, sha, check)
	}
	key := RepoLockKey(repo)
	clone := SharedClonePath(g.reposRootFor(dir), repo)
	// every write here is the shared clone's: locked under its root
	root := g.sharedLockRoot(clone, repo)
	if err := g.syncSharedClone(ctx, root, key, repo, clone); err != nil {
		return err
	}
	spec, ref := fetchRefspec(branch)
	if _, err := g.fetchLocked(ctx, root, key, clone, "git", "fetch", "--quiet", "origin", spec); err != nil {
		return err
	}
	if err := check(clone, ref); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	unlock, err := lockRepo(ctx, root, key)
	if err != nil {
		return err
	}
	defer unlock()
	// only dir's own stale registration goes: a blanket prune would take
	// a live item's worktree a pod's mounts had hidden (forgetStaleRegistrations)
	if err := forgetStaleRegistrations(clone, dir, ""); err != nil {
		return err
	}
	_, err = g.run(ctx, clone, "git", "worktree", "add", "--quiet", "--detach", dir, sha)
	return err
}

// addDetachedClone is addDetachedWorktree for a dir in a job (ADR-039):
// repo cloned into dir alone, branch fetched into it, check asked of
// it, sha checked out detached — no shared clone, no other job's
// objects. The clone is cloneOwn's, as a build's checkout is.
func (g *Git) addDetachedClone(ctx context.Context, repo, dir, branch, sha string, check func(clone, ref string) error) error {
	root, key := g.lockRoot("", dir), RepoLockKey(repo)
	if err := g.cloneOwn(ctx, root, key, repo, dir, "", "--no-checkout"); err != nil {
		return err
	}
	spec, ref := fetchRefspec(branch)
	if _, err := g.fetchLocked(ctx, root, key, dir, "git", "fetch", "--quiet", "origin", spec); err != nil {
		return err
	}
	if err := check(dir, ref); err != nil {
		return err
	}
	_, err := g.run(ctx, dir, "git", "checkout", "--quiet", "--detach", sha)
	return err
}
