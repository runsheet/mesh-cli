package node

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LeftoverCommitMessage is the message of the commit the node makes of
// what a build session left uncommitted.
const LeftoverCommitMessage = "leftover changes from the build session"

// commitLeftovers commits what the build session left in dir beyond its
// HEAD — tracked changes and untracked files git does not ignore — as
// one commit, LeftoverCommitMessage, before the validation runs: the
// validation, the attestation and the changeset (the finish takes
// HEAD's tree) are then all of the one commit, and a builder that
// finished without committing loses nothing. It runs before any suite,
// so no validation output is among what it stages; the node's own
// context under ContextDir is never staged (excluded via info/exclude
// when it is written, and left out here besides). The commit is the session's agent's — the
// checkout's user.name/user.email, set by Identify, and ident's env
// besides — and passes through the checkout's hooks (core.hooksPath,
// HooksDir), so it carries the pairers' trailers and never a Claude
// one. It answers the paths committed, none when the checkout was
// clean. A checkout in the middle of a merge, rebase, cherry-pick or
// revert is left as it is: committing it would finish the operation the
// session left, not record its work.
func commitLeftovers(ctx context.Context, dir string, ident Identity) ([]string, error) {
	git := func(stdin string, args ...string) (string, error) {
		cmd := hardenedGitCmdEnv(ctx, "", dir, ident.Env(), args...)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return string(out), nil
	}
	status, err := git("", "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	// every path status names (a rename's both), but the node's context
	var stage []string
	nodeContext := filepath.ToSlash(ContextDir) + "/"
	entries := strings.Split(status, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		ps := []string{e[3:]}
		if e[0] == 'R' || e[0] == 'C' {
			i++ // the rename's source follows
			if i < len(entries) && entries[i] != "" {
				ps = append(ps, entries[i])
			}
		}
		for _, p := range ps {
			if !strings.HasPrefix(p, nodeContext) {
				stage = append(stage, p)
			}
		}
	}
	if len(stage) == 0 {
		return nil, nil
	}
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		p, err := git("", "rev-parse", "--git-path", name)
		if err != nil {
			continue
		}
		p = strings.TrimSpace(p)
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if _, err := os.Stat(p); err == nil {
			return nil, fmt.Errorf("the checkout is mid-operation (%s); its uncommitted changes are left as they are", name)
		}
	}
	// the paths as literal pathspecs, read from stdin: no argument list
	// to outgrow, no glob in a file name to widen it
	if _, err := git(strings.Join(stage, "\x00")+"\x00", "--literal-pathspecs", "add", "--all", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return nil, err
	}
	names, err := git("", "diff", "--cached", "--name-only", "-z", "--no-renames")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(names, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	if _, err := git("", "commit", "--quiet", "-m", LeftoverCommitMessage); err != nil {
		return nil, err
	}
	return paths, nil
}
