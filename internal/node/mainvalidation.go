package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

// candidateWorktrees makes and removes a candidate's checkout (*Git).
type candidateWorktrees interface {
	AddCandidateWorktree(ctx context.Context, repo, dir, branch, sha string) error
	RemoveWorkspace(ctx context.Context, dir string) error
}

// ErrNoCandidateWorktrees is a Runner whose Workspacer cannot make a
// candidate's worktree.
var ErrNoCandidateWorktrees = errors.New("the node's workspace makes no candidate worktrees")

// validateCandidate validates batch's candidate and uploads its
// attestation: the candidate branch fetched on tok (the repo's token;
// nil is the operator's own credentials, the fallback) into a fresh
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
func (r *Runner) validateCandidate(ctx context.Context, batch gateway.MainValidationBatch, tok *gateway.GitHubToken) (validate.Attestation, error) {
	wt, ok := r.WS.(candidateWorktrees)
	if !ok {
		return validate.Attestation{}, ErrNoCandidateWorktrees
	}
	label := fmt.Sprintf("batch-%d", batch.Number)
	prefix := fmt.Sprintf("merge batch %d (%s)", batch.Number, batch.RepoFullName)
	dir := CandidateDir(r.WorkRoot, batch)
	vr := r.validator()
	t := validationTarget{
		item:    gateway.WorkItem{OrgID: batch.OrgID, RepoID: batch.RepoID},
		dir:     dir,
		base:    batch.DefaultBranch,
		prefix:  prefix,
		label:   label,
		session: func() string { return "" },
		redact:  func(err error) error { return err },
		report:  func(_ context.Context, note string) { r.logf("%s: %s", prefix, note) },
		batchID: batch.MergeBatchID,
		repoTok: tok,
	}
	defer func() {
		if err := wt.RemoveWorkspace(context.WithoutCancel(ctx), dir); err != nil {
			r.logf("%s: remove %s: %v", prefix, dir, err)
		}
		_ = os.RemoveAll(sessionunit.Dir(dir))
		_ = os.Remove(dir + ".attestation.json")
	}()
	r.logf("%s: validating candidate %s at %s", prefix, batch.CandidateBranch, short(batch.SHA))
	if err := wt.AddCandidateWorktree(vr.onItemToken(ctx, t), batch.RepoFullName, dir, batch.CandidateBranch, batch.SHA); err != nil {
		var he *CandidateHeadError
		if errors.As(err, &he) {
			r.logf("%s: %v", prefix, err)
			return validate.Attestation{}, err
		}
		return validate.Attestation{}, validate.Classify(fmt.Errorf("fetch candidate %s: %w", batch.CandidateBranch, err))
	}
	if _, err := validate.Load(dir); err != nil {
		// no validate.yml on the candidate is nothing to attest: the caller's
		return validate.Attestation{}, err
	}
	pod, err := OpenSessionPod(ctx, r.Sandbox, dir, label, CandidateKind, sandbox.GitIdentity{}, r.logf)
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
	if att.Commit != batch.SHA {
		return att, fmt.Errorf("validated %s, not the candidate's %s", short(att.Commit), short(batch.SHA))
	}
	if failed, ok := failedStep(att); ok && failed.EnvironmentKind != "" {
		vf := &validationFailure{Step: failed}
		r.logf("%s: %s; not attested", prefix, validate.Reason(vf))
		return att, vf
	}
	out := vr.upload(ctx, t, att)
	if out.Err != nil {
		return att, out.Err
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
	if _, err := os.Stat(dir); err == nil {
		if err := g.RemoveWorkspace(ctx, dir); err != nil {
			return err
		}
	}
	root, key := g.lockRoot("", dir), RepoLockKey(repo)
	clone := SharedClonePath(g.reposRootFor(dir), repo)
	if err := g.syncSharedClone(ctx, root, key, repo, clone); err != nil {
		return err
	}
	ref := "refs/remotes/origin/" + branch
	if _, err := g.fetchLocked(ctx, root, key, clone, "git", "fetch", "--quiet", "origin", "+refs/heads/"+branch+":"+ref); err != nil {
		return err
	}
	head, err := g.run(ctx, clone, "git", "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return err
	}
	if head != sha {
		return &CandidateHeadError{Branch: branch, Want: sha, Got: head}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	unlock, err := lockRepo(ctx, root, key)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := g.run(ctx, clone, "git", "worktree", "prune"); err != nil {
		return err
	}
	_, err = g.run(ctx, clone, "git", "worktree", "add", "--quiet", "--detach", dir, sha)
	return err
}
