package node

import "context"

// AddRebaseWorktree checks branch out at dir as a fresh detached worktree
// of the repo's shared clone at origin/<branch>, as AddCandidateWorktree
// checks a candidate out: the branch fetched into the shared clone, the
// ref checked, `git worktree add --detach`. It reads nothing of the
// item's own checkout, so a rebase runs whether that directory is there,
// gone, or its worktree admin lost. The session pushes HEAD to
// refs/heads/<branch>.
func (g *Git) AddRebaseWorktree(ctx context.Context, repo, dir, branch string) error {
	ref := "refs/remotes/origin/" + branch
	return g.addDetachedWorktree(ctx, repo, dir, branch, ref, func(clone, ref string) error {
		_, err := g.run(ctx, clone, "git", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		return err
	})
}
