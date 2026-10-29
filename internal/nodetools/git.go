package nodetools

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// envUnset are the variables that could hand git a credential of the
// node's own: none reaches a tool's git (the relay holds the lease's
// tokens).
var envUnset = []string{"GIT_ASKPASS", "SSH_ASKPASS", "GH_TOKEN", "GITHUB_TOKEN", "GIT_CONFIG_PARAMETERS"}

// hardened are git's -c flags for a command the node runs in a checkout
// the session has written to: no credential helper or askpass, no hook
// and no fsmonitor of the session's (each would run the session's
// command as the node), no proxy and no ssh command.
var hardened = []string{
	"-c", "credential.helper=", "-c", "core.askPass=", "-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false", "-c", "http.proxy=", "-c", "core.sshCommand=",
}

// gitEnv is the environment a tool's git runs in: the node's, without
// envUnset, with no system or global config, never prompting.
func gitEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(envUnset, name) {
			out = append(out, kv)
		}
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
}

// gitCmd is one hardened git command in dir, extra added to its
// environment.
func gitCmd(ctx context.Context, dir string, extra []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append(append([]string(nil), hardened...), args...)...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(), extra...)
	return cmd
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
