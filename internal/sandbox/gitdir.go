package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitdir.go finds the shared git directory a session's workspace hangs
// off (mesh-specs ADR 017, One worktree per item): an item's checkout is
// a git worktree of the repo's one clone, its `.git` a file pointing into
// the clone's `.git/worktrees/<id>`, so the pod must mount the clone's
// git directory beside the worktree at the same path for the worktree to
// resolve inside it. A checkout from before the layout is a full clone,
// its `.git` a directory, and needs nothing mounted beside it.

// GitCommonDir is the absolute, symlink-resolved git common directory of
// the worktree at workspace — what `git rev-parse --git-common-dir`
// answers there, resolved against the workspace when relative — or ""
// when workspace is a full clone, its `.git` a directory.
func GitCommonDir(workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("git common dir of %s: %w", workspace, err)
	}
	info, err := os.Stat(filepath.Join(abs, ".git"))
	if err != nil {
		return "", fmt.Errorf("git common dir of %s: %w", abs, err)
	}
	if info.IsDir() {
		return "", nil
	}
	cmd := exec.Command("git", "-C", abs, "rev-parse", "--git-common-dir")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git common dir of %s: git rev-parse: %w: %s", abs, err, strings.TrimSpace(stderr.String()))
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("git common dir of %s: git rev-parse answered nothing", abs)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(abs, dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("git common dir of %s: %w", abs, err)
	}
	return resolved, nil
}

// GitDir is the absolute, symlink-resolved git directory of the checkout
// at workspace — what `git rev-parse --absolute-git-dir` answers there:
// for a worktree its own admin entry, `<common>/worktrees/<id>`, which
// holds its HEAD, index and logs.
func GitDir(workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("git dir of %s: %w", workspace, err)
	}
	cmd := exec.Command("git", "-C", abs, "rev-parse", "--absolute-git-dir")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git dir of %s: git rev-parse: %w: %s", abs, err, strings.TrimSpace(stderr.String()))
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("git dir of %s: git rev-parse answered nothing", abs)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("git dir of %s: %w", abs, err)
	}
	return resolved, nil
}
