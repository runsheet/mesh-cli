package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RebaseSuffix ends a rebase checkout's directory outside a job:
// <root>/<goal>/<seq>-rebase; in a job it is <job>/rebase.
const RebaseSuffix = "-rebase"

// RebaseWorkspaceSpec is a rebase's own checkout of the item item names:
// a fresh detached clone of its own at <job>/rebase, or
// <root>/<goal>/<seq>-rebase beside the build's <root>/<goal>/<seq>
// outside a job (ADR-028, A rebase gets its own worktree; ADR-039),
// so a build checkout deleted, swept or left pointing at a pruned
// worktree registration is never read, written or authorized by the
// rebase. Root and Repos stay the item's, so the repo lock is the
// build's; no shared clone is read or written.
func RebaseWorkspaceSpec(item WorkspaceSpec) WorkspaceSpec {
	s := item
	if item.Job != "" {
		s.Path = filepath.Join(item.Job, "rebase")
		return s
	}
	s.Path = filepath.Join(item.Root, item.GoalSlug, fmt.Sprintf("%d%s", item.Seq, RebaseSuffix))
	return s
}

// isRebaseDir is whether dir is a rebase checkout (RebaseWorkspaceSpec):
// an adopted session an older node ran in the item's own checkout is
// not, and that checkout is never removed after it.
func isRebaseDir(dir string) bool {
	dir = filepath.Clean(dir)
	if _, _, ok := jobOf(dir); ok && filepath.Base(dir) == "rebase" {
		return true
	}
	return strings.HasSuffix(dir, RebaseSuffix)
}

// rebaseWorktrees makes and removes a rebase's checkout (*Git).
type rebaseWorktrees interface {
	AddRebaseWorktree(ctx context.Context, repo, dir, branch string) error
	RemoveWorkspace(ctx context.Context, dir string) error
}

// ErrNoRebaseWorktrees is a Rebaser whose Workspacer cannot make a
// rebase's worktree.
var ErrNoRebaseWorktrees = errors.New("the node's workspace makes no rebase worktrees")

// fetchRefspec is the refspec addDetachedClone fetches branch by, and
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

// addDetachedClone checks sha out detached at dir in a clone of its own
// (ADR-039): a checkout a crashed run left at dir removed first, repo
// cloned into dir alone, branch fetched into it, check asked of it, sha
// checked out — no shared clone, no worktree, no other job's objects.
// The clone is cloneOwn's, as a build's checkout is.
func (g *Git) addDetachedClone(ctx context.Context, repo, dir, branch, sha string, check func(clone, ref string) error) error {
	if _, err := os.Stat(dir); err == nil {
		if err := g.RemoveWorkspace(ctx, dir); err != nil {
			return err
		}
	}
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

// AddRebaseWorktree checks branch out at dir as a fresh detached clone
// of its own at origin/<branch>, as a candidate's is checked out in a
// job: a stale dir there removed first, then addDetachedClone — repo
// cloned into dir by cloneOwn, the branch fetched into it, the ref
// checked, `git checkout --detach` — in a job or outside one alike, so
// no shared clone is written and no worktree registered (ADR-039,
// Nothing is shared between jobs). It reads nothing of the item's own
// checkout, so a rebase runs whether that directory is there, gone, or
// its worktree admin lost. The session pushes HEAD to refs/heads/<branch>.
func (g *Git) AddRebaseWorktree(ctx context.Context, repo, dir, branch string) error {
	if _, err := os.Stat(dir); err == nil {
		if err := g.RemoveWorkspace(ctx, dir); err != nil {
			return err
		}
	}
	ref := "refs/remotes/origin/" + branch
	return g.addDetachedClone(ctx, repo, dir, branch, ref, func(clone, ref string) error {
		_, err := g.run(ctx, clone, "git", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		return err
	})
}
