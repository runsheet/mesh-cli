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
