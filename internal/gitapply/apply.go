// Package gitapply applies changes onto a commit without a worktree
// (runsheet ADR 077): each change is three-way merged onto the tip with
// git merge-tree and written as a commit with commit-tree, under a fixed
// identity, so two nodes applying the same changes onto the same commit
// write the same shas. It touches no ref, no index and no working tree;
// the caller moves what it wants to the result.
//
// It is generic: it knows nothing of items, pull requests or the
// platform, only commits.
package gitapply

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Change is one change to apply: the diff Base..Head. Base "" is the
// merge base of Head and the tip it is applied onto. Message is the
// commit's message; "" is Head's own message in Linear mode and
// "Merge <Head>" in Merge mode.
type Change struct {
	Head, Base, Message string
}

// Identity is the author and committer of every commit Apply writes.
// A fixed When makes the commits deterministic; the zero When is git's
// now.
type Identity struct {
	Name, Email string
	When        time.Time
}

// Mode is how a change's commit is parented.
type Mode int

const (
	// Linear parents each commit on the tip alone: the change rebased,
	// squashed to one commit (an item's rebase).
	Linear Mode = iota
	// Merge parents each commit on the tip and the change's Head: the
	// change merged (pull requests, chains).
	Merge
)

func (m Mode) String() string {
	switch m {
	case Linear:
		return "linear"
	case Merge:
		return "merge"
	}
	return "mode(" + strconv.Itoa(int(m)) + ")"
}

// Step is one change applied: the commit written, its tree, and the
// paths it changes from the tip it was applied onto.
type Step struct {
	Commit, Tree string
	Paths        []string
}

// Conflict is the change that did not apply: its index in the changes,
// its Head, and the paths git could not merge.
type Conflict struct {
	Index int
	Head  string
	Paths []string
}

func (c *Conflict) Error() string {
	return fmt.Sprintf("change %d (%s) conflicts with the tip: %s", c.Index, c.Head, strings.Join(c.Paths, ", "))
}

// MinGit is the oldest git Apply runs on: merge-tree's --merge-base
// came in 2.40.
const MinGit = "2.40"

// ErrGitTooOld is the environment error Apply answers on a git older
// than MinGit; the caller falls back to what it did before.
var ErrGitTooOld = errors.New("gitapply: git " + MinGit + " or later is needed")

// Apply applies changes, in order, onto the commit onto names in the
// repository at dir, each onto the last one's commit, and answers the
// steps it wrote. A change that does not merge stops it: the steps
// before it and the *Conflict, with a nil error. Any other failure,
// a git older than MinGit (ErrGitTooOld) among them, is the error.
//
// Every object Apply reads must already be in dir's repository; Apply
// fetches nothing and moves no ref.
func Apply(ctx context.Context, dir, onto string, changes []Change, mode Mode, id Identity) ([]Step, *Conflict, error) {
	if mode != Linear && mode != Merge {
		return nil, nil, fmt.Errorf("gitapply: unknown %s", mode)
	}
	if err := checkVersion(ctx, dir); err != nil {
		return nil, nil, err
	}
	tip, err := revParse(ctx, dir, onto+"^{commit}")
	if err != nil {
		return nil, nil, err
	}
	var steps []Step
	for i, c := range changes {
		head, err := revParse(ctx, dir, c.Head+"^{commit}")
		if err != nil {
			return steps, nil, fmt.Errorf("change %d: %w", i, err)
		}
		base := c.Base
		if base == "" {
			// the merge base with the tip, not with onto: in Merge mode a
			// change stacked on an earlier one finds that one's Head; in
			// Linear mode the two are the same commit
			out, err := git(ctx, dir, nil, "merge-base", head, tip)
			if err != nil {
				return steps, nil, fmt.Errorf("change %d: merge base of %s and %s: %w", i, head, tip, err)
			}
			base = strings.TrimSpace(string(out))
		} else if base, err = revParse(ctx, dir, base+"^{commit}"); err != nil {
			return steps, nil, fmt.Errorf("change %d: %w", i, err)
		}
		tree, paths, err := mergeTree(ctx, dir, base, tip, head)
		if err != nil {
			return steps, nil, fmt.Errorf("change %d: %w", i, err)
		}
		if paths != nil {
			return steps, &Conflict{Index: i, Head: head, Paths: paths}, nil
		}
		msg, err := message(ctx, dir, c, head, mode)
		if err != nil {
			return steps, nil, fmt.Errorf("change %d: %w", i, err)
		}
		args := []string{"commit-tree", "--no-gpg-sign", "-p", tip}
		if mode == Merge {
			args = append(args, "-p", head)
		}
		args = append(args, "-F", "-", tree)
		out, err := gitIn(ctx, dir, identityEnv(id), msg, args...)
		if err != nil {
			return steps, nil, fmt.Errorf("change %d: %w", i, err)
		}
		commit := strings.TrimSpace(string(out))
		changed, err := diffPaths(ctx, dir, tip, commit)
		if err != nil {
			return steps, nil, fmt.Errorf("change %d: %w", i, err)
		}
		steps = append(steps, Step{Commit: commit, Tree: tree, Paths: changed})
		tip = commit
	}
	return steps, nil, nil
}

// mergeTree merges base..head onto tip and answers the tree written;
// paths are the conflicted ones, nil when it merged cleanly.
func mergeTree(ctx context.Context, dir, base, tip, head string) (string, []string, error) {
	cmd := exec.CommandContext(ctx, "git", "-c", "merge.renames=true", "merge-tree", "--write-tree", "--name-only", "-z", "--no-messages", "--merge-base="+base, tip, head)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	conflicted := errors.As(err, &exit) && exit.ExitCode() == 1
	if err != nil && !conflicted {
		return "", nil, fmt.Errorf("git merge-tree %s..%s onto %s: %w: %s", base, head, tip, err, strings.TrimSpace(stderr.String()))
	}
	fields := strings.Split(strings.TrimRight(stdout.String(), "\x00"), "\x00")
	tree := fields[0]
	if tree == "" {
		return "", nil, fmt.Errorf("git merge-tree %s..%s onto %s wrote no tree", base, head, tip)
	}
	if !conflicted {
		return tree, nil, nil
	}
	paths := []string{}
	for _, p := range fields[1:] {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return tree, paths, nil
}

// message is c's commit message: its own, else Head's in Linear mode
// and "Merge <Head>" in Merge mode.
func message(ctx context.Context, dir string, c Change, head string, mode Mode) (string, error) {
	if c.Message != "" {
		return c.Message, nil
	}
	if mode == Merge {
		return "Merge " + head + "\n", nil
	}
	out, err := git(ctx, dir, nil, "log", "-1", "--format=%B", head)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// diffPaths are the paths commit changes from tip.
func diffPaths(ctx context.Context, dir, tip, commit string) ([]string, error) {
	out, err := git(ctx, dir, nil, "diff-tree", "-r", "-z", "--no-renames", "--name-only", tip, commit)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// identityEnv is id as git's author and committer environment.
func identityEnv(id Identity) []string {
	env := []string{
		"GIT_AUTHOR_NAME=" + id.Name, "GIT_AUTHOR_EMAIL=" + id.Email,
		"GIT_COMMITTER_NAME=" + id.Name, "GIT_COMMITTER_EMAIL=" + id.Email,
	}
	if !id.When.IsZero() {
		when := fmt.Sprintf("%d %s", id.When.Unix(), id.When.Format("-0700"))
		env = append(env, "GIT_AUTHOR_DATE="+when, "GIT_COMMITTER_DATE="+when)
	}
	return env
}

func revParse(ctx context.Context, dir, rev string) (string, error) {
	out, err := git(ctx, dir, nil, "rev-parse", "--verify", "--quiet", rev)
	if err != nil {
		return "", fmt.Errorf("%s names no commit: %w", strings.TrimSuffix(rev, "^{commit}"), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// git runs git in dir with env added.
func git(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	return gitIn(ctx, dir, env, "", args...)
}

// gitIn runs git as git does, stdin on its standard input.
func gitIn(ctx context.Context, dir string, env []string, stdin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

var versionRE = regexp.MustCompile(`^git version (\d+)\.(\d+)`)

// checkVersion answers ErrGitTooOld on a git older than MinGit, or one
// whose version it cannot read.
func checkVersion(ctx context.Context, dir string) error {
	out, err := git(ctx, dir, nil, "version")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrGitTooOld, err)
	}
	return versionOK(strings.TrimSpace(string(out)))
}

// versionOK answers ErrGitTooOld when v, git version's words, names a
// git older than MinGit or none it can read.
func versionOK(v string) error {
	m := versionRE.FindStringSubmatch(v)
	if m == nil {
		return fmt.Errorf("%w: cannot read %q", ErrGitTooOld, v)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 2 || major == 2 && minor < 40 {
		return fmt.Errorf("%w: %s", ErrGitTooOld, v)
	}
	return nil
}
