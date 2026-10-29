package nodev1

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/resources"
)

// gitEnvUnset are the variables that could hand git or a session a
// credential of the node's own: none reaches a lease's git or session.
var gitEnvUnset = []string{"GIT_ASKPASS", "SSH_ASKPASS", "GH_TOKEN", "GITHUB_TOKEN", "GIT_CONFIG_PARAMETERS"}

// gitEnv is the environment a lease's git runs in: the node's, without
// gitEnvUnset, never prompting.
func gitEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, u := range gitEnvUnset {
			if name == u {
				drop = true
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return append(out, "GIT_TERMINAL_PROMPT=0")
}

// noCredentials are git's -c flags that clear every credential helper
// and askpass the node's own git config may name.
var noCredentials = []string{"-c", "credential.helper=", "-c", "core.askPass="}

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

// checkout clones the lease's repository through its grant into
// workspace and checks out the brief's base: origin is the relay's
// capability URL, and no credential helper, askpass or token is in the
// checkout's config or git's environment. With mirrors, the tenant's
// mirror of the repository is first brought up to date through the same
// grant and the clone borrows its objects, then copies them
// (--dissociate), so the checkout never depends on the mirror; a mirror
// that cannot be synced is skipped and logged.
func checkout(ctx context.Context, grant *gitrelay.Grant, b *meshapi.Brief, workspace string, mirrors *resources.Mirrors, logf func(string, ...any)) error {
	git := func(dir string, args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append(append([]string(nil), noCredentials...), args...)...)
		cmd.Dir = dir
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(lastLines(string(out), 5)))
		}
		return nil
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
	if err := git(workspace, append(clone, origin, ".")...); err != nil {
		return err
	}
	if b.Code == nil {
		return nil
	}
	if ref := b.Code.BaseRef; ref != "" {
		if err := git(workspace, "fetch", "-q", "origin", ref); err != nil {
			return err
		}
	}
	target := b.Code.BaseSHA
	if target == "" && b.Code.BaseRef != "" {
		target = "FETCH_HEAD"
	}
	if target != "" {
		if err := git(workspace, "checkout", "-q", "--detach", target); err != nil {
			return err
		}
	}
	return resumePushed(ctx, git, b, workspace)
}

// resumePushed moves a retried write lease's checkout onto its job's
// push_to when an earlier lease of the job already pushed there and the
// push is on top of the base: a lease that failed after its push landed
// is handed back (report), and a run from the base again could only
// push a non-fast-forward the relay refuses. The first lease, a read
// lease and a job naming no push_to keep the base.
func resumePushed(ctx context.Context, git func(string, ...string) error, b *meshapi.Brief, workspace string) error {
	if b.LeaseN <= 1 || b.Code.PushTo == "" || b.Git.Access != string(gitrelay.Write) {
		return nil
	}
	ref := b.Code.PushTo
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	if err := git(workspace, "fetch", "-q", "origin", ref); err != nil {
		return nil // nothing was pushed there
	}
	if err := git(workspace, "merge-base", "--is-ancestor", "HEAD", "FETCH_HEAD"); err != nil {
		return nil // not on top of the base: the session starts from the base
	}
	return git(workspace, "checkout", "-q", "--detach", "FETCH_HEAD")
}
