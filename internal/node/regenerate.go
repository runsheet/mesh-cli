package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"projectmesh.io/mesh-cli/internal/validate"
)

// regenKind is how the mechanical path of a rebase ended (goal
// a-generated-file-conflict-is-regenerated-not-rebased-by-hand).
type regenKind string

const (
	// RegenClean: the rebase onto the base had no conflict at all.
	RegenClean regenKind = "clean"
	// RegenRegenerated: every conflict was confined to the declared
	// generated paths, they were regenerated and every check passed.
	RegenRegenerated regenKind = "regenerated"
	// RegenNotGenerated: a conflicted path is not a generated one; the
	// rebase was aborted and is a session's to do.
	RegenNotGenerated regenKind = "not_generated"
	// RegenCheckFailed: a generate or check command failed; the
	// worktree was put back at origin/<branch>.
	RegenCheckFailed regenKind = "check_failed"
)

// regenOutcome is what regenerateRebase did. Head is the rebased HEAD
// for Clean and Regenerated; Paths the generated paths resolved
// (Regenerated) or the conflicted paths that are not generated
// (NotGenerated); Command and Output the failing command and its
// output's tail (CheckFailed).
type regenOutcome struct {
	Kind    regenKind
	Head    string
	Paths   []string
	Command string
	Output  string
}

// String is the outcome as one log line's tail.
func (o regenOutcome) String() string {
	switch o.Kind {
	case RegenClean:
		return fmt.Sprintf("rebased cleanly to %s", short(o.Head))
	case RegenRegenerated:
		return fmt.Sprintf("regenerated %s; rebased to %s", strings.Join(o.Paths, ", "), short(o.Head))
	case RegenNotGenerated:
		return fmt.Sprintf("conflict outside the generated paths: %s", strings.Join(o.Paths, ", "))
	case RegenCheckFailed:
		return fmt.Sprintf("%s failed: %s", o.Command, o.Output)
	}
	return string(o.Kind)
}

// regenOutputTail is how much of a failing command's output the
// outcome keeps.
const regenOutputTail = 2000

// regenerateRebase is the mechanical path of a rebase: no harness, no
// account. dir is the rebase worktree, detached at origin/<branch> as
// Rebaser.Rebase makes it; run runs gen's commands in dir. It rebases
// onto origin/<base>, and at each commit the rebase stops on resolves a
// conflict confined to gen's paths by taking the base's side of each
// and running gen.Generate, then, once the rebase completes, runs
// gen.Check: success is claimed only when every check passes, so a
// silent auto-merge duplicate is caught by the repo's build or vet. A
// conflict outside gen's paths aborts the rebase (NotGenerated); a
// failing generate or check puts the worktree back at origin/<branch>
// (CheckFailed). Nothing is pushed here. An error is git itself
// failing, the worktree put back where it can be.
func regenerateRebase(ctx context.Context, dir, branch, base string, gen validate.Generated, run validate.Runner) (regenOutcome, error) {
	origin := "origin/" + branch
	restore := func() {
		if regenRebaseInProgress(ctx, dir) {
			_, _ = regenGit(ctx, dir, "rebase", "--abort")
		}
		_, _ = regenGit(ctx, dir, "reset", "--quiet", "--hard", origin)
	}

	conflicted := false
	resolved := map[string]bool{}
	_, rebaseErr := regenGit(ctx, dir, "rebase", "origin/"+base)
	for rebaseErr != nil {
		if !regenRebaseInProgress(ctx, dir) {
			restore()
			return regenOutcome{}, fmt.Errorf("rebase onto origin/%s: %w", base, rebaseErr)
		}
		unmerged, err := regenUnmerged(ctx, dir)
		if err != nil {
			restore()
			return regenOutcome{}, err
		}
		if len(unmerged) == 0 {
			// Stopped with nothing unmerged: the commit came out empty
			// once its generated files were taken from the base, so
			// there is nothing of it left to replay.
			if staged, err := regenGit(ctx, dir, "diff", "--cached", "--name-only"); err == nil && strings.TrimSpace(staged) == "" {
				_, rebaseErr = regenGit(ctx, dir, "rebase", "--skip")
				continue
			}
			restore()
			return regenOutcome{}, fmt.Errorf("rebase onto origin/%s stopped with no conflict: %w", base, rebaseErr)
		}
		conflicted = true
		var other []string
		for _, p := range unmerged {
			if !gen.Covers(p) {
				other = append(other, p)
			}
		}
		if len(other) > 0 {
			restore()
			return regenOutcome{Kind: RegenNotGenerated, Paths: other}, nil
		}
		for _, p := range unmerged {
			// During a rebase the sides are swapped: "ours" is the
			// upstream being rebased onto — origin/<base>, main — and
			// "theirs" the branch's commit being replayed. Main's side
			// is what the generator is run over.
			if _, err := regenGit(ctx, dir, "checkout", "--ours", "--", p); err != nil {
				// No base side (main deleted it): drop it, and let the
				// generator make it again if it still should exist.
				if _, err := regenGit(ctx, dir, "rm", "--quiet", "-f", "--", p); err != nil {
					restore()
					return regenOutcome{}, fmt.Errorf("resolve %s: %w", p, err)
				}
			}
			resolved[p] = true
		}
		if cmd, out, failed := regenRunAll(ctx, run, gen.Generate, gen); failed {
			restore()
			return regenOutcome{Kind: RegenCheckFailed, Command: cmd, Output: out}, nil
		}
		if err := regenStageGenerated(ctx, dir, gen); err != nil {
			restore()
			return regenOutcome{}, err
		}
		_, rebaseErr = regenGit(ctx, dir, "rebase", "--continue")
	}

	if !conflicted {
		head, err := gitHead(ctx, dir)
		if err != nil {
			return regenOutcome{}, err
		}
		return regenOutcome{Kind: RegenClean, Head: head}, nil
	}
	if cmd, out, failed := regenRunAll(ctx, run, gen.Check, gen); failed {
		restore()
		return regenOutcome{Kind: RegenCheckFailed, Command: cmd, Output: out}, nil
	}
	head, err := gitHead(ctx, dir)
	if err != nil {
		return regenOutcome{}, err
	}
	paths := make([]string, 0, len(resolved))
	for p := range resolved {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return regenOutcome{Kind: RegenRegenerated, Head: head, Paths: paths}, nil
}

// regenRunAll runs cmds in order through run, each on gen's timeout,
// and names the first that fails with its output's tail.
func regenRunAll(ctx context.Context, run validate.Runner, cmds []string, gen validate.Generated) (string, string, bool) {
	for _, c := range cmds {
		var out bytes.Buffer
		code, err := run.Run(ctx, c, gen.Timeout, &out, &out)
		if err != nil || code != 0 {
			text := out.String()
			if err != nil {
				text = strings.TrimSpace(text + "\n" + err.Error())
			}
			if len(text) > regenOutputTail {
				text = "…" + text[len(text)-regenOutputTail:]
			}
			return c, fmt.Sprintf("exit %d: %s", code, strings.TrimSpace(text)), true
		}
	}
	return "", "", false
}

// regenStageGenerated stages every change under gen's paths the
// generators left — modified, new or deleted — and nothing else.
func regenStageGenerated(ctx context.Context, dir string, gen validate.Generated) error {
	out, err := regenGit(ctx, dir, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	var paths []string
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		status, p := e[:2], e[3:]
		if status[0] == 'R' || status[0] == 'C' {
			i++ // the rename's source follows
		}
		if gen.Covers(p) {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	_, err = regenGit(ctx, dir, append([]string{"add", "-A", "--"}, paths...)...)
	return err
}

// regenUnmerged lists the paths the stopped rebase left unmerged.
func regenUnmerged(ctx context.Context, dir string) ([]string, error) {
	out, err := regenGit(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths, nil
}

// regenRebaseInProgress reports whether dir's worktree has a rebase
// stopped in it.
func regenRebaseInProgress(ctx context.Context, dir string) bool {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		p, err := regenGit(ctx, dir, "rev-parse", "--git-path", name)
		if err != nil {
			continue
		}
		p = strings.TrimSpace(p)
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// regenGit runs git in dir with no editor, so a `rebase --continue`
// keeps the replayed commit's message.
func regenGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return stdout.String(), fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), exit.ExitCode(), msg)
		}
		return stdout.String(), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}
