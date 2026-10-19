package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// CheckoutName is the checkout step's name, as Run records it.
const CheckoutName = "checkout"

// Prepare makes an item's workspace on the node with tok on each git
// command alone — never in a config file — and returns its directory:
// node.Git.Checkout over the item's spec.
type Prepare func(ctx context.Context, tok *gateway.GitHubToken) (string, error)

// Checkout is the job's first step: the item's workspace prepared on the
// node by prepare, on tok, with no credential left in it, so the job
// container that mounts it holds no GitHub token (runsheet
// adr-061-mesh-is-a-job-system §3). The directory goes on
// state.WorkspaceDir. The step then reads back every git config the
// workspace carries — its own, and for a worktree its config.worktree
// and the shared clone's config — and fails if any names the token, so
// a regression is the step's failure, not a leak.
func Checkout(prepare Prepare, tok *gateway.GitHubToken) Step {
	return Step{Name: CheckoutName, Run: func(ctx context.Context, state *State) error {
		if tok == nil || tok.Token == "" {
			return errors.New("checkout: no github token")
		}
		dir, err := prepare(ctx, tok)
		if err != nil {
			return fmt.Errorf("checkout: %w", err)
		}
		if path, err := ConfigHolding(dir, tok.Token); err != nil {
			return fmt.Errorf("checkout: %w", err)
		} else if path != "" {
			return fmt.Errorf("checkout: %s holds the item's github token", path)
		}
		state.WorkspaceDir = dir
		return nil
	}}
}

// ConfigHolding is the first git config of the workspace dir that holds
// secret — .git/config for a clone; for a worktree its gitdir's
// config.worktree and the shared clone's config — or "" when none does.
func ConfigHolding(dir, secret string) (string, error) {
	paths, err := ConfigPaths(dir)
	if err != nil {
		return "", err
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if strings.Contains(string(b), secret) {
			return p, nil
		}
	}
	return "", nil
}

// ConfigPaths are the git config files the workspace dir reads: its
// .git/config for a full clone; for a worktree — .git a file naming
// `gitdir: <clone>/.git/worktrees/<n>` — that gitdir's config.worktree
// and the shared clone's config (its commondir's).
func ConfigPaths(dir string) ([]string, error) {
	gitDir, common, err := GitDirs(dir)
	if err != nil {
		return nil, err
	}
	if gitDir == common {
		return []string{filepath.Join(common, "config")}, nil
	}
	return []string{filepath.Join(gitDir, "config.worktree"), filepath.Join(common, "config")}, nil
}

// GitDirs is the workspace dir's git dir and common dir: both dir/.git
// for a full clone; for a worktree the gitdir its .git file names and
// the commondir that gitdir names.
func GitDirs(dir string) (gitDir, common string, err error) {
	dotGit := filepath.Join(dir, ".git")
	fi, err := os.Stat(dotGit)
	if err != nil {
		return "", "", err
	}
	if fi.IsDir() {
		return dotGit, dotGit, nil
	}
	b, err := os.ReadFile(dotGit)
	if err != nil {
		return "", "", err
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok {
		return "", "", fmt.Errorf("%s: not a gitdir file", dotGit)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	common = gitDir
	if c, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common = strings.TrimSpace(string(c))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
	}
	return filepath.Clean(gitDir), filepath.Clean(common), nil
}
