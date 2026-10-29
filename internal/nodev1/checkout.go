package nodev1

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"projectmesh.io/mesh-cli/internal/githarden"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/resources"
)

// gitEnvUnset are the variables that could hand git or a session a
// credential of the node's own: none reaches a lease's git or session.
var gitEnvUnset = []string{"GIT_ASKPASS", "SSH_ASKPASS", "GH_TOKEN", "GITHUB_TOKEN", "GIT_CONFIG_PARAMETERS"}

// leaseGit is one git command in dir, a lease's checkout, hardened
// (internal/githarden): its environment is the node's without any
// credential of the node's or system and global config, and no hook,
// fsmonitor, filter, driver or credential helper the checkout's config
// names runs as the node — a parked thread's or retried lease's
// checkout is one a session wrote to.
func leaseGit(ctx context.Context, dir string, args ...string) *exec.Cmd {
	return githarden.Command(ctx, dir, nil, args...)
}

// policyOf is the relay policy of the brief's git: its vendor and base,
// its repository, read or write, and the refs a push may touch, each
// fast-forward-only unless the brief's rule says false. Only a write
// policy names push rules, and only a write policy has its relay mint a
// write token (MintSource with the policy's access).
func policyOf(g *meshapi.Git) gitrelay.Policy {
	p := gitrelay.Policy{Repo: g.Repo, Access: gitrelay.Read, Vendor: g.Vendor, Base: baseOf(g)}
	if g.Access == string(gitrelay.Write) {
		p.Access = gitrelay.Write
		for _, r := range g.Push {
			ff := r.FastForwardOnly == nil || *r.FastForwardOnly
			p.Push = append(p.Push, gitrelay.RefRule{Ref: r.Ref, CreateOnly: r.CreateOnly, FastForwardOnly: ff})
		}
	}
	return p
}

// sourceRequest is the mint of the brief's git token: the vendor and
// base_url exactly as the brief's git has them — Latchkey compares them
// with the job's git as Mesh answers it — (none for GitHub at its own
// origin), the repository, and the policy's access.
func sourceRequest(g *meshapi.Git, p gitrelay.Policy) SourceRequest {
	return SourceRequest{Vendor: g.Vendor, BaseURL: g.BaseURL, Repository: g.Repo, Access: string(p.Access)}
}

// baseOf is the vendor origin the brief's git names: its base_url, else
// https://<host>, else "" for the vendor's own.
func baseOf(g *meshapi.Git) string {
	switch {
	case g.BaseURL != "":
		return g.BaseURL
	case g.Host != "":
		return "https://" + g.Host
	}
	return ""
}

// resumePoint is where a retried lease's checkout starts instead of the
// base: a ref an earlier lease of the job pushed, and its commit. LeaseN
// is the lease that pushed it, 0 when only push_to says so.
type resumePoint struct {
	Ref    string
	SHA    string
	LeaseN int
}

// checkout clones the lease's repository through its grant into
// workspace and checks out the brief's base — or, for a retried write
// lease, what an earlier lease of the job pushed (resumePushed), which it
// answers: origin is the relay's capability URL, and no credential
// helper, askpass or token is in the checkout's config or git's
// environment. With mirrors, the tenant's mirror of the repository is
// first brought up to date through the same grant and the clone borrows
// its objects, then copies them (--dissociate), so the checkout never
// depends on the mirror; a mirror that cannot be synced is skipped and
// logged.
func checkout(ctx context.Context, grant *gitrelay.Grant, b *meshapi.Brief, workspace string, mirrors *resources.Mirrors, logf func(string, ...any)) (*resumePoint, error) {
	git := func(dir string, args ...string) (string, error) {
		out, err := leaseGit(ctx, dir, args...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(lastLines(string(out), 5)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	origin := grant.Base() + b.Git.Repo + ".git"
	clone := []string{"clone", "-q"}
	if mirrors != nil {
		pol := grant.Policy()
		key := resources.MirrorKey{Tenant: b.Tenant, Vendor: pol.Vendor, Base: pol.Base, Repo: b.Git.Repo}
		if dir, err := mirrors.Sync(ctx, key, origin); err != nil {
			logf("nodev1: lease %s: the mirror of %s is not used: %v", b.LeaseID, b.Git.Repo, err)
		} else {
			clone = append(clone, "--reference", dir, "--dissociate")
		}
	}
	if _, err := git(workspace, append(clone, origin, ".")...); err != nil {
		return nil, err
	}
	if b.Code == nil {
		return nil, nil
	}
	if ref := b.Code.BaseRef; ref != "" {
		if _, err := git(workspace, "fetch", "-q", "origin", ref); err != nil {
			return nil, err
		}
	}
	target := b.Code.BaseSHA
	if target == "" && b.Code.BaseRef != "" {
		target = "FETCH_HEAD"
	}
	if target != "" {
		if _, err := git(workspace, "checkout", "-q", "--detach", target); err != nil {
			return nil, err
		}
	}
	return resumePushed(git, b, workspace, logf)
}

// resumePushed moves a retried write lease's checkout onto what an
// earlier lease of its job pushed, when that is on top of the base, and
// answers where it moved to. A lease that pushed and then ended without
// finishing leaves its push at the vendor, and a run from the base again
// could only push a non-fast-forward the relay refuses.
//
// The brief's pushed (Mesh's record of the earlier leases' ok pushes,
// from their relays' AccessRecords) names the refs, the most recent
// last: the checkout starts from that ref as the vendor holds it now, so
// a ref that moved on since still gives a fast-forward. A brief from a
// Mesh that names no pushed falls back to the job's push_to (#1678). The
// first lease, a read lease, and a push that is not on top of the base
// keep the base.
func resumePushed(git func(string, ...string) (string, error), b *meshapi.Brief, workspace string, logf func(string, ...any)) (*resumePoint, error) {
	if b.Git == nil || b.Git.Access != string(gitrelay.Write) {
		return nil, nil
	}
	var at resumePoint
	switch {
	case len(b.Pushed) > 0:
		p := b.Pushed[len(b.Pushed)-1]
		at = resumePoint{Ref: p.Ref, SHA: p.SHA, LeaseN: p.LeaseN}
	case b.LeaseN > 1 && b.Code.PushTo != "":
		at = resumePoint{Ref: b.Code.PushTo}
	default:
		return nil, nil
	}
	if !strings.HasPrefix(at.Ref, "refs/") {
		at.Ref = "refs/heads/" + at.Ref
	}
	if _, err := git(workspace, "fetch", "-q", "origin", at.Ref); err != nil {
		if at.LeaseN > 0 {
			logf("nodev1: lease %s: %s, which lease %d pushed, is gone: the session starts from the base", b.LeaseID, at.Ref, at.LeaseN)
		}
		return nil, nil // nothing is there now
	}
	if _, err := git(workspace, "merge-base", "--is-ancestor", "HEAD", "FETCH_HEAD"); err != nil {
		logf("nodev1: lease %s: %s is not on top of the base: the session starts from the base", b.LeaseID, at.Ref)
		return nil, nil
	}
	if _, err := git(workspace, "checkout", "-q", "--detach", "FETCH_HEAD"); err != nil {
		return nil, err
	}
	sha, err := git(workspace, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	at.SHA = sha
	return &at, nil
}
