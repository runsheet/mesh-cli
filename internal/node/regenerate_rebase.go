package node

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/validate"
)

// regenWorkspace is what the mechanical path of a rebase needs of the
// node's Workspacer beyond its own (*Git): the base fetched into the
// rebase worktree, and the rebased HEAD pushed to the branch with a
// lease on what origin had.
type regenWorkspace interface {
	FetchBase(ctx context.Context, dir, base string) error
	PushRebased(ctx context.Context, dir, branch string) error
}

// FetchBase fetches origin's base into dir's refs/remotes/origin/<base>
// under the repo's fetch lock, so the rebase is onto what origin has.
func (g *Git) FetchBase(ctx context.Context, dir, base string) error {
	_, err := g.fetchLocked(ctx, g.lockRoot("", dir), g.repoKeyOf(ctx, dir), dir, "git", "fetch", "--quiet", "origin", "+refs/heads/"+base+":refs/remotes/origin/"+base)
	return err
}

// PushRebased pushes dir's detached HEAD to refs/heads/<branch> with
// --force-with-lease=<branch>:origin/<branch>, as a rebase session is
// told to: a branch that moved on origin since the worktree was made is
// refused, never overwritten.
func (g *Git) PushRebased(ctx context.Context, dir, branch string) error {
	out, err := g.run(ctx, dir, "git", "push", "--quiet", "--force-with-lease="+branch+":origin/"+branch, "origin", "HEAD:refs/heads/"+branch)
	if err != nil && PushRefusal(out) {
		return &PushRefusedError{Branch: branch, Output: strings.TrimSpace(out), Err: err}
	}
	return err
}

// tryRegenerate is a rebase's mechanical path, tried before any session
// (goal a-generated-file-conflict-is-regenerated-not-rebased-by-hand,
// docs/adr/adr-038-a-generated-only-conflict-is-regenerated.md): the
// rebase kind with harness none of runsheet ADR 061. When the worktree's
// validate.yml declares `generated`, regenerateRebase runs its generate
// and check commands through the validation's runner — on the host, or
// in a pod of the org's sandbox, never the host on a sandboxed node —
// and a conflict confined to the generated paths whose check passes is
// pushed with a lease, attested and finished with no account opened, no
// session and no model cost. done is whether the rebase ended here
// (err its end, as Rebase's); otherwise the reason is said in one line,
// the worktree is back at origin/<branch> and the session runs as
// before.
func (r *Rebaser) tryRegenerate(ctx context.Context, rb *rebaseRun, cred *ItemCred, dir, branch, base string) (done bool, err error) {
	label := rb.label
	cfg, err := validate.Load(dir)
	if errors.Is(err, validate.ErrNoValidation) {
		return false, nil
	}
	fallThrough := func(why string) (bool, error) {
		r.logf("item %s: regenerate: %s; a rebase session instead", label, why)
		rb.report(ctx, "regenerate: "+why+"; a rebase session instead")
		// what the session starts from is what it always has: the
		// branch as origin has it, nothing a generator left behind
		_, _ = regenGit(ctx, dir, "reset", "--quiet", "--hard", "origin/"+branch)
		_, _ = regenGit(ctx, dir, "clean", "-fdq")
		return false, nil
	}
	if err != nil {
		return fallThrough(fmt.Sprintf("validate.yml: %v", err))
	}
	if !cfg.Generated.Declared() {
		return false, nil
	}
	g, ok := r.WS.(regenWorkspace)
	if !ok {
		r.logf("item %s: regenerate: the node's workspace cannot push a rebased head; a rebase session instead", label)
		return false, nil
	}
	tok, err := cred.ForFetch(ctx)
	if err != nil {
		return fallThrough(fmt.Sprintf("github token: %v", err))
	}
	if err := g.FetchBase(OnItemToken(ctx, r.WS, tok), dir, base); err != nil {
		return fallThrough(fmt.Sprintf("fetch origin/%s: %v", base, err))
	}
	vr := r.validator()
	v, err := vr.start(ctx, validationTarget{item: rb.item, dir: dir, base: base, prefix: "rebase " + label, label: label + " regenerate", report: rb.report, cred: cred})
	if err != nil {
		return fallThrough(fmt.Sprintf("runner: %v", err))
	}
	if v == nil {
		return false, nil
	}
	out, err := regenerateRebase(ctx, dir, branch, base, cfg.Generated, v.runner)
	v.close()
	if err != nil {
		return fallThrough(err.Error())
	}
	if out.Kind != RegenRegenerated {
		if out.Kind == RegenClean {
			return fallThrough("the rebase onto origin/" + base + " had no conflict to regenerate")
		}
		return fallThrough(out.String())
	}
	paths := strings.Join(out.Paths, ", ")
	if tok, err = cred.ForPush(ctx); err != nil {
		return fallThrough(fmt.Sprintf("github token for the push: %v", err))
	}
	pctx := OnItemToken(ctx, r.WS, tok)
	if err := g.PushRebased(pctx, dir, branch); err != nil {
		return fallThrough(fmt.Sprintf("push %s: %v", branch, err))
	}
	rb.cred, rb.dir, rb.branch, rb.base = cred, dir, branch, base
	fail := func(cause error) (bool, error) { return true, rb.fail(ctx, cause) }
	// what origin has is the head, not what was pushed
	head, err := r.WS.PushedHead(pctx, dir, branch, base)
	if err != nil {
		return fail(fmt.Errorf("pushed %s by regenerating %s, but: %w", short(out.Head), paths, err))
	}
	if head != out.Head {
		return fail(fmt.Errorf("pushed %s by regenerating %s, but origin/%s is at %s: the push did not land", short(out.Head), paths, branch, short(head)))
	}
	rb.report(ctx, fmt.Sprintf("rebased %s onto %s at %s by regenerating %s (no session)", branch, base, short(head), paths))
	// the rebased head is a commit no attestation names (ADR 032), in a
	// pod of its own on a sandboxed node: there is no session's
	r.validateRebased(ctx, rb, nil, head)
	ctx = gateway.WithNodeID(ctx, nodeIDOf(r.Scopes, rb.item.OrgID, r.Cfg))
	if err := r.GW.FinishWorkItemRebase(ctx, rb.item.ID, head, out.Paths, nil); err != nil {
		return fail(fmt.Errorf("the rebase landed at %s, but finishing it failed: %w", short(head), err))
	}
	r.logf("item %s: rebased %s@%s by regenerating %d file(s); no session", label, branch, short(head), len(out.Paths))
	return true, nil
}
