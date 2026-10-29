// Package githarden runs git on the host in a checkout a session may
// have written to, so nothing of the checkout's own config runs as the
// node (Mesh v1 spec, "The git relay and the Latchkey token relay": no
// credential ever reaches a checkout, and nothing in one reaches the
// node). A session that can write a checkout can write its .git/config:
// a hook, an fsmonitor, a filter, a merge or diff driver, a credential
// helper or an ssh command there would each run its command as the node
// the next time the node ran git in it. Every git command nodev1, its
// node tools, the git relay and gitapply run in such a checkout is made
// here.
package githarden

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// envUnset are the variables that could hand git a credential of the
// node's own or config of its caller's: none reaches a hardened git.
var envUnset = []string{"GIT_ASKPASS", "SSH_ASKPASS", "GH_TOKEN", "GITHUB_TOKEN", "GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_COUNT", "GIT_EXTERNAL_DIFF", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_PROXY_COMMAND", "GIT_EDITOR", "GIT_SEQUENCE_EDITOR"}

// args are git's -c flags for every hardened command: no credential
// helper or askpass, no hook and no fsmonitor of the checkout's, no
// proxy, no ssh command, no ext:: transport, no pack-objects hook and
// no signing program.
var args = []string{
	"-c", "credential.helper=", "-c", "core.askPass=", "-c", "core.hooksPath=" + os.DevNull,
	"-c", "core.fsmonitor=false", "-c", "http.proxy=", "-c", "core.sshCommand=",
	"-c", "protocol.ext.allow=never", "-c", "uploadpack.packObjectsHook=",
	"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false",
}

// Args are the hardened -c flags, a copy the caller may append to.
func Args() []string {
	return append([]string(nil), args...)
}

// Env is the environment a hardened git runs in: base (the node's
// environment when nil) without envUnset, with no system or global
// config and never prompting.
func Env(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	var out []string
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(envUnset, name) && !strings.HasPrefix(name, "GIT_CONFIG_KEY_") && !strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			out = append(out, kv)
		}
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}

// Command is one hardened git command in dir, extra added to its
// environment (last wins). Past Args, every command driver the
// checkout's config names — a filter's clean, smudge or process, a merge
// driver, a diff textconv or command, a URL's credential helper, a
// remote's upload or receive pack — is cleared, so a .gitattributes
// that names it runs nothing (an emptied filter passes content through;
// an emptied merge or diff driver fails the command). A checkout whose
// config cannot be read answers a command that fails to start with
// why, never one run on it unread.
func Command(ctx context.Context, dir string, extra []string, gitArgs ...string) *exec.Cmd {
	flags := Args()
	drivers, err := driverOverrides(ctx, dir)
	flags = append(flags, drivers...)
	cmd := exec.CommandContext(ctx, "git", append(flags, gitArgs...)...)
	cmd.Dir = dir
	cmd.Env = append(Env(nil), extra...)
	if err != nil {
		cmd.Err = err
	}
	return cmd
}

// driverOverrides are the -c flags that clear each command driver the
// config git reads in dir names: the local and worktree config and
// whatever they include, as git itself would read it there. A dir
// outside any repository (a clone's empty target) names none.
func driverOverrides(ctx context.Context, dir string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", append(Args(), "config", "--name-only", "--list")...)
	cmd.Dir = dir
	cmd.Env = Env(nil)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("githarden: reading the checkout's git config: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var flags []string
	seen := map[string]bool{}
	for _, key := range strings.Split(string(out), "\n") {
		key = strings.TrimSpace(key)
		if key == "" || seen[strings.ToLower(key)] || !driverKey(key) {
			continue
		}
		seen[strings.ToLower(key)] = true
		flags = append(flags, "-c", key+"=")
	}
	return flags, nil
}

// driverKey says whether key, a config key as git lists it
// (section.subsection.name, the section and name lowercased), names a
// command git would run: a filter, merge or diff driver's, a URL's
// credential helper, a remote's upload or receive pack.
func driverKey(key string) bool {
	section, rest, ok := strings.Cut(key, ".")
	if !ok {
		return false
	}
	i := strings.LastIndex(rest, ".")
	if i < 0 {
		return false
	}
	name := strings.ToLower(rest[i+1:])
	switch strings.ToLower(section) {
	case "filter":
		return name == "clean" || name == "smudge" || name == "process"
	case "merge":
		return name == "driver"
	case "diff":
		return name == "textconv" || name == "command"
	case "credential":
		return name == "helper"
	case "remote":
		return name == "uploadpack" || name == "receivepack"
	}
	return false
}
