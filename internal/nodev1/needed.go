package nodev1

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/nodetools"
)

// A job says what must still be true when it starts, and the node
// checks it before it starts the lease's work (ADR-052;
// projectmesh-io/api ADR 001 amendment, a job is still needed or ends
// quietly). A validation of a pull request's head is pointless once the
// head has moved; one of a merge candidate, once a newer candidate
// replaced it. The check is a read of the code host's refs through the
// lease's own git grant — the relay the checkout itself goes through,
// on the token the relay mints for it — made after the grant opens and
// before the checkout, the pod and the session. A precondition that no
// longer holds ends the job no_longer_needed: no hand-back, no failure.

// neededWithin bounds the whole check: a code host that does not answer
// leaves the job to run.
const neededWithin = 30 * time.Second

// notNeeded is why b's job is no longer needed — the first of its
// preconditions that no longer holds — or "" when each holds or cannot
// be told. Only a ref the code host answered for decides: a
// precondition of a type this node does not know, on a repository the
// lease's grant does not reach, on a vendor that keeps no ref for a
// change, or whose read failed, is logged and taken as holding, so the
// job runs as it would have without it. dir is where git runs: the
// lease's own directory, never a checkout.
func (r *Runtime) notNeeded(ctx context.Context, grant *gitrelay.Grant, b *meshapi.Brief, dir string) string {
	if len(b.Preconditions) == 0 {
		return ""
	}
	if grant == nil {
		r.logf("nodev1: lease %s: its job names %d precondition(s) but no git, so none is checked", b.LeaseID, len(b.Preconditions))
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, neededWithin)
	defer cancel()
	git := leaseGitRun(ctx)
	pol := grant.Policy()
	for _, p := range b.Preconditions {
		ref, what := preconditionRef(pol.Vendor, p)
		if ref == "" {
			r.logf("nodev1: lease %s: precondition %s on %s is not checked: %s", b.LeaseID, p.Type, p.Repo, what)
			continue
		}
		repo, ok := grantRepo(pol, p.Repo)
		if !ok {
			r.logf("nodev1: lease %s: precondition %s is not checked: the lease's git does not reach %s", b.LeaseID, p.Type, p.Repo)
			continue
		}
		out, err := git(dir, "ls-remote", grant.Base()+repo+".git", ref)
		if err != nil {
			r.logf("nodev1: lease %s: precondition %s on %s is not checked: %v", b.LeaseID, p.Type, p.Repo, err)
			continue
		}
		at := ""
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[1] == ref {
				at = f[0]
			}
		}
		switch {
		case at == "":
			return fmt.Sprintf("%s is gone; the job was to run while it was %s", what, p.SHA)
		case !strings.EqualFold(at, strings.TrimSpace(p.SHA)):
			return fmt.Sprintf("%s is now %s, not %s", what, at, p.SHA)
		}
	}
	return ""
}

// preconditionRef is the ref whose head p names, as the vendor keeps
// it, and what to call it in a reason: a ref_head's own ref, a pr_head's
// change ref (GitHub's refs/pull/<n>/head, GitLab's
// refs/merge-requests/<n>/head; nodetools.ChangeRef). ref is "" for a
// precondition that cannot be read as a ref, and what then says why.
func preconditionRef(vendor string, p meshapi.Precondition) (ref, what string) {
	switch p.Type {
	case meshapi.PreconditionRefHead:
		if ref = fullRef(p.Ref); ref == "" {
			return "", "it names no ref"
		}
		return ref, p.Repo + " " + ref
	case meshapi.PreconditionPRHead:
		if p.Number <= 0 {
			return "", "it names no pull request"
		}
		ref = nodetools.ChangeRef(vendor, fmt.Sprintf("refs/pull/%d/head", p.Number))
		if ref == "" {
			return "", "the code host keeps no ref for a pull request's head"
		}
		return ref, fmt.Sprintf("the head of pull request %s#%d", p.Repo, p.Number)
	}
	return "", "this node does not know the type"
}

// grantRepo is repo as the grant's policy names it — its own repository
// or one of its read repos — and whether the grant reaches it at all.
func grantRepo(pol gitrelay.Policy, repo string) (string, bool) {
	repo = strings.TrimSuffix(strings.TrimSpace(repo), ".git")
	if strings.EqualFold(repo, pol.Repo) {
		return pol.Repo, true
	}
	if i := slices.IndexFunc(pol.ReadRepos, func(r string) bool { return strings.EqualFold(r, repo) }); i >= 0 {
		return pol.ReadRepos[i], true
	}
	return "", false
}

// noLongerNeeded reports the lease's job no longer needed: the job ends
// quietly at Mesh, which counts no hand-back and no failure for it.
func (r *Runtime) noLongerNeeded(id, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	clipped := clip(r.redact(reason), maxReason)
	st, err := r.API.NoLongerNeeded(ctx, id, meshapi.NoLongerNeededRequest{Reason: clipped})
	if err != nil && meshapi.LeaseGone(err) {
		r.logf("nodev1: lease %s was no longer the node's to end as no longer needed", id)
		return
	}
	if err != nil {
		r.logf("nodev1: lease %s: no-longer-needed: %v", id, err)
		return
	}
	r.logf("nodev1: lease %s is no longer needed and did not start: %s; job %s is %s", id, clip(clipped, 300), st.JobID, st.Status)
}
