// Package githardentest plants, in a checkout, what a session could
// write to its own .git to have the node run a command of its: a hook in
// .git/hooks, a core.hooksPath of its own, a core.fsmonitor, a
// credential.helper and a core.sshCommand (mesh-specs
// mesh-v1-gaps-for-products.md, gap 7). Each planted command touches a
// marker when it runs, so a test of a host git path can say that none of
// them ran there.
package githardentest

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Hooks are the hooks planted, in .git/hooks and in the planted
// core.hooksPath: every one a host git a node runs (clone, fetch,
// checkout, commit, push, a ref update, gc) would reach.
var Hooks = []string{"post-checkout", "pre-commit", "commit-msg", "post-commit", "pre-push", "post-merge",
	"post-rewrite", "reference-transaction", "post-index-change", "pre-auto-gc", "push-to-checkout"}

// Plant is what Plant planted in one checkout.
type Plant struct {
	// Dir is the checkout.
	Dir string
	// Markers is where each planted command leaves its marker.
	Markers string
	// HooksPath is the planted core.hooksPath.
	HooksPath string
}

// New plants, in the checkout at dir, a hook of each of Hooks in its
// .git/hooks and in a core.hooksPath of its own, a core.fsmonitor, a
// credential.helper and a core.sshCommand, each touching its marker
// ("hook-<name>", "hookspath-<name>", "fsmonitor", "credential",
// "sshcommand") under the plant's Markers when it runs.
func New(t testing.TB, dir string) *Plant {
	t.Helper()
	p := &Plant{Dir: dir, Markers: t.TempDir(), HooksPath: filepath.Join(t.TempDir(), "session-hooks")}
	gitDir := gitDirOf(t, dir)
	hooks := filepath.Join(gitDir, "hooks")
	for _, d := range []string{hooks, p.HooksPath} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range Hooks {
		p.script(t, filepath.Join(hooks, h), "hook-"+h)
		p.script(t, filepath.Join(p.HooksPath, h), "hookspath-"+h)
	}
	scripts := t.TempDir()
	for _, kv := range [][2]string{
		{"core.hooksPath", p.HooksPath},
		{"core.fsmonitor", p.script(t, filepath.Join(scripts, "fsmonitor"), "fsmonitor")},
		{"credential.helper", p.script(t, filepath.Join(scripts, "credential"), "credential")},
		{"core.sshCommand", p.script(t, filepath.Join(scripts, "sshcommand"), "sshcommand")},
	} {
		Git(t, dir, "config", kv[0], kv[1])
	}
	return p
}

// script writes an executable at path that touches marker and exits 0
// (a failing hook would stop the git that ran it before the next hook
// could), and answers path.
func (p *Plant) script(t testing.TB, path, marker string) string {
	t.Helper()
	body := "#!/bin/sh\ntouch '" + filepath.Join(p.Markers, marker) + "'\nexit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Ran are the markers of the planted commands that ran, sorted.
func (p *Plant) Ran() []string {
	entries, _ := os.ReadDir(p.Markers)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// NoneRan fails t, naming them, when any planted command ran.
func (p *Plant) NoneRan(t testing.TB, path string) {
	t.Helper()
	if ran := p.Ran(); len(ran) > 0 {
		t.Fatalf("%s ran what the session planted in its checkout on the host: %v", path, ran)
	}
}

// Reset forgets every marker, so a test can plant before a session's own
// git and count only what runs after.
func (p *Plant) Reset(t testing.TB) {
	t.Helper()
	if err := os.RemoveAll(p.Markers); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.Markers, 0o755); err != nil {
		t.Fatal(err)
	}
}

// AskingServer is an http git remote that asks for credentials on every
// request, so a git that has a credential helper runs it.
func AskingServer(t testing.TB) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/acme/fixture.git"
}

// SSHRemote is an ssh remote nothing listens on: a git that has a
// core.sshCommand runs it to reach it.
const SSHRemote = "ssh://git@127.0.0.1:1/acme/fixture.git"

// Git runs git in dir as a session would, on none of the caller's
// config, and fails t when it fails.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=s", "GIT_AUTHOR_EMAIL=s@x", "GIT_COMMITTER_NAME=s", "GIT_COMMITTER_EMAIL=s@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitDirOf is the checkout's own git directory.
func gitDirOf(t testing.TB, dir string) string {
	t.Helper()
	d := Git(t, dir, "rev-parse", "--absolute-git-dir")
	if d == "" {
		t.Fatalf("%s is not a checkout", dir)
	}
	return d
}
