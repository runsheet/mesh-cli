package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// hardenedGitCmd is git as the node runs it on the host in a workspace a
// session may have written: nothing the repository's config or hooks
// name runs, and nothing of the operator's environment but what git
// needs reaches it.
//
// The command line overrides what .git/config could turn into a command
// (hardenedGitArgs). The environment is hardenedGitEnv's allowlist with
// the prompt off, the system config skipped and the credential helpers
// reset, then the item token's helper (gitTokenEnv), else the helper
// the node itself wrote into dir (writeHelper, nodeHelpers), else the
// operator's own gh for base (operatorHelper) — the old platform's and
// a tokenless main validation's fallback, which the reset would
// otherwise leave with no credential at all. The reset rides
// GIT_CONFIG_COUNT and not `-c credential.helper=`: git reads the
// environment's entries before the command line's, so a `-c` reset would
// drop the token's helper too (TestHardenedGitKeepsItemTokenHelper).
func hardenedGitCmd(ctx context.Context, base, dir string, args ...string) *exec.Cmd {
	return hardenedGitCmdEnv(ctx, base, dir, nil, args...)
}

// hardenedGitCmdEnv is hardenedGitCmd with extra variables set on top of
// the hardened environment, last wins — a commit's identity, an editor,
// the served commit's fixed GIT_* — named by the caller, never
// os.Environ(). Every git the node execs under internal/node is made
// here (TestNoGitExecBypassesHardenedRunner).
func hardenedGitCmdEnv(ctx context.Context, base, dir string, extra []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append(hardenedGitArgs(dir), args...)...)
	cmd.Dir = dir
	cmd.Env = append(hardenedGitEnv(ctx, base, dir), extra...)
	return cmd
}

// hardenedGitArgs are the -c overrides every host git command carries:
// no fsmonitor, no ssh command, no ext:: transport, no pack-objects
// hook, and hooks only from the node's own hooks dir — HooksDir(dir),
// which the node writes and a pod mounts read-only (ADR-024), when it
// exists, else none at all.
func hardenedGitArgs(dir string) []string {
	hooks := os.DevNull
	if dir != "" {
		if fi, err := os.Stat(HooksDir(dir)); err == nil && fi.IsDir() {
			hooks = HooksDir(dir)
		}
	}
	return []string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + hooks,
		"-c", "core.sshCommand=",
		"-c", "protocol.ext.allow=never",
		"-c", "uploadpack.packObjectsHook=",
	}
}

// gitEnvAllowed are the variables of the node's environment a host git
// command keeps: where to find programs and home, the locale, the temp
// dir, TLS roots and proxies, the session bus a keyring-held gh login
// is read through (operatorHelper), and the commit identity a test or an
// operator sets. HOME stays, so ~/.gitconfig is still read — for
// everything but its credential helpers, which the reset drops. No GH_*
// and no other GIT_*: GIT_CONFIG_GLOBAL,
// GIT_DIR, GIT_SSH_COMMAND and the rest never reach it.
var gitEnvAllowed = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "LANG": true, "LANGUAGE": true,
	"TMPDIR": true, "TZ": true, "XDG_CONFIG_HOME": true,
	"XDG_RUNTIME_DIR": true, "DBUS_SESSION_BUS_ADDRESS": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
	"GIT_AUTHOR_NAME": true, "GIT_AUTHOR_EMAIL": true, "GIT_AUTHOR_DATE": true,
	"GIT_COMMITTER_NAME": true, "GIT_COMMITTER_EMAIL": true, "GIT_COMMITTER_DATE": true,
}

// hardenedGitEnv is the allowlisted environment (gitEnvAllowed, LC_*)
// plus GIT_TERMINAL_PROMPT=0, GIT_CONFIG_NOSYSTEM=1 and the config
// entries: credential.helper reset, then the item token's helper when
// ctx carries one (withGitToken), else the one the node wrote into dir,
// else the operator's gh for base (GitHubURL when empty).
func hardenedGitEnv(ctx context.Context, base, dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if gitEnvAllowed[k] || strings.HasPrefix(k, "LC_") {
			env = append(env, kv)
		}
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	pairs := [][2]string{{"credential.helper", ""}}
	t, ok := ctx.Value(gitTokenKey{}).(gitToken)
	if !ok {
		t, ok = nodeHelperOf(dir)
	}
	if !ok {
		t, ok = operatorHelper(base)
	}
	if ok {
		key := "credential." + t.base + ".helper"
		if t.relay != "" {
			// through the node's git relay (runsheet ADR 076): GitHub's
			// URL points at it and no helper answers, so the token is in
			// no environment or config the command or its children see
			pairs = append(pairs, [2]string{key, ""}, [2]string{"url." + t.relay + ".insteadOf", t.base + "/"})
		} else {
			pairs = append(pairs, [2]string{key, ""}, [2]string{key, t.helper})
		}
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(pairs)))
	for i, p := range pairs {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+p[0], "GIT_CONFIG_VALUE_"+n+"="+p[1])
	}
	return env
}

// operatorHelper is the helper a tokenless command answers with: the
// operator's own gh at its own config dir (OperatorGHConfigDir), as
// AuthorizeOperator writes it and the operator's global `gh auth
// git-credential` answered before the reset, for base alone. Not
// GH_TOKEN: the git child never sees it.
func operatorHelper(base string) (gitToken, bool) {
	if base == "" {
		base = GitHubURL
	}
	ghDir, err := OperatorGHConfigDir()
	if err != nil || strings.ContainsAny(ghDir, "'\n") {
		return gitToken{}, false
	}
	return gitToken{base: strings.TrimRight(base, "/"), helper: "!GH_CONFIG_DIR='" + ghDir + "' gh auth git-credential"}, true
}

// nodeHelpers is the credential helper the node wrote into each
// checkout's own config (writeHelper), held in memory: the reset drops
// every helper .git/config names, the node's own among them, and a
// session can rewrite that file, so the node answers with what it wrote
// and never reads it back. A node that restarts has none until it
// writes one again (ForPush, Refresh); its pushes and validation
// fetches run on OnItemToken anyway.
var nodeHelpers = struct {
	sync.Mutex
	byDir map[string]gitToken
}{byDir: map[string]gitToken{}}

func rememberHelper(dir, base, helper string) {
	nodeHelpers.Lock()
	defer nodeHelpers.Unlock()
	nodeHelpers.byDir[filepath.Clean(dir)] = gitToken{base: strings.TrimRight(base, "/"), helper: helper}
}

func forgetHelper(dir string) {
	nodeHelpers.Lock()
	defer nodeHelpers.Unlock()
	delete(nodeHelpers.byDir, filepath.Clean(dir))
}

func nodeHelperOf(dir string) (gitToken, bool) {
	if dir == "" {
		return gitToken{}, false
	}
	nodeHelpers.Lock()
	defer nodeHelpers.Unlock()
	t, ok := nodeHelpers.byDir[filepath.Clean(dir)]
	return t, ok
}

// ghCmd is a gh (or any other non-git) command realRun runs: the node's
// own environment, which gh needs for the operator's login. gh is only
// run before a session, on a workspace no session has written.
func ghCmd(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), gitTokenEnv(ctx)...)
	return cmd
}
