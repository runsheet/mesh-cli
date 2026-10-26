package node

import (
	"context"
	"os"
	"path/filepath"
)

// prepareWorktree makes spec's checkout a worktree of its shared clone,
// as Prepare did before a checkout was a clone of its own (ADR-039): the
// worktree machinery — RebuildWorktree, lockWorktree, RemoveWorkspace,
// the stale-worktree checks — still serves a worktree made then, and its
// tests start from one. A checkout already there is left to Prepare.
func prepareWorktree(ctx context.Context, g *Git, spec WorkspaceSpec) (string, error) {
	dir := spec.Dir()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return g.Prepare(ctx, spec)
	}
	clone, key := g.seedClone(spec), RepoLockKey(spec.Repo)
	root := g.sharedLockRoot(clone, spec.Repo)
	if err := g.syncSharedClone(ctx, root, key, spec.Repo, clone); err != nil {
		return "", err
	}
	base := spec.BaseRef
	if base == "" {
		base = "main"
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	unlock, err := lockRepo(ctx, root, key)
	if err != nil {
		return "", err
	}
	defer unlock()
	for _, kv := range [][2]string{{"extensions.worktreeConfig", "true"}, {"gc.worktreePruneExpire", "never"}} {
		if _, err := g.run(ctx, clone, "git", "config", "--local", kv[0], kv[1]); err != nil {
			return "", err
		}
	}
	if err := forgetStaleRegistrations(clone, dir, spec.Branch); err != nil {
		return "", err
	}
	args := []string{"worktree", "add", "--quiet", "--no-track", "-B", spec.Branch, dir, "origin/" + base}
	if _, err := g.run(ctx, clone, "git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+spec.Branch); err == nil {
		args = []string{"worktree", "add", "--quiet", "--track", "-B", spec.Branch, dir, "origin/" + spec.Branch}
	} else if _, err := g.run(ctx, clone, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+spec.Branch); err == nil {
		args = []string{"worktree", "add", "--quiet", dir, spec.Branch}
	}
	if _, err := g.run(ctx, clone, "git", args...); err != nil {
		return "", err
	}
	return dir, g.lockWorktree(ctx, dir, spec)
}
