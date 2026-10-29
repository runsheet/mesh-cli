package nodetools

import (
	"context"
	"os/exec"
	"strings"

	"projectmesh.io/mesh-cli/internal/githarden"
)

// gitCmd is one git command in dir, a checkout the session has written
// to, extra added to its environment: hardened (internal/githarden), so
// no hook, fsmonitor, filter, driver or credential helper of the
// session's runs as the node, and no credential of the node's reaches
// git (the relay holds the lease's tokens).
func gitCmd(ctx context.Context, dir string, extra []string, args ...string) *exec.Cmd {
	return githarden.Command(ctx, dir, extra, args...)
}

// git runs one hardened git command in dir, answering its output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitCmd(ctx, dir, nil, args...).CombinedOutput()
	return string(out), err
}

// lastLines is the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
