package githarden

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// plainGit runs git in dir as a session would, with nothing hardened.
func plainGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=s", "GIT_AUTHOR_EMAIL=s@x", "GIT_COMMITTER_NAME=s", "GIT_COMMITTER_EMAIL=s@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// plantedCheckout is a checkout whose session planted, in its own
// .git/config, every command git would run as whoever runs git there —
// a hooks path of its own, an fsmonitor, a filter on every file, a merge
// and a diff driver, a URL's credential helper — each touching a marker
// in ran when it runs. It answers the checkout and its two commits.
func plantedCheckout(t *testing.T, ran string) (dir, first, second string) {
	t.Helper()
	dir = t.TempDir()
	plainGit(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	plainGit(t, dir, "add", "-A")
	plainGit(t, dir, "commit", "-qm", "one")
	first = plainGit(t, dir, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644)
	plainGit(t, dir, "commit", "-qam", "two")
	second = plainGit(t, dir, "rev-parse", "HEAD")

	script := func(name string) string {
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, []byte("#!/bin/sh\ntouch "+filepath.Join(ran, name)+"\ncat\n"), 0o755)
		return p
	}
	hooks := filepath.Join(dir, ".session-hooks")
	os.MkdirAll(hooks, 0o755)
	for _, h := range []string{"post-checkout", "pre-commit", "post-commit", "reference-transaction", "post-index-change"} {
		os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\ntouch "+filepath.Join(ran, h)+"\n"), 0o755)
	}
	os.WriteFile(filepath.Join(dir, ".git", "info", "attributes"), []byte("* filter=planted merge=planted diff=planted\n"), 0o644)
	for _, kv := range [][2]string{
		{"core.hooksPath", hooks},
		{"core.fsmonitor", script("fsmonitor")},
		{"filter.planted.clean", script("clean")},
		{"filter.planted.smudge", script("smudge")},
		{"filter.planted.process", script("process")},
		{"filter.planted.required", "false"},
		{"merge.planted.driver", script("merge")},
		{"diff.planted.textconv", script("textconv")},
		{"credential.https://example.com.helper", script("credential")},
	} {
		plainGit(t, dir, "config", kv[0], kv[1])
	}
	return dir, first, second
}

// A hardened git in a checkout a session planted commands in runs none
// of them, through status (fsmonitor, clean), checkout (smudge,
// post-checkout), merge-base, and the plumbing a commit is made with.
func TestHardenedGitRunsNothingTheCheckoutPlanted(t *testing.T) {
	ran := t.TempDir()
	dir, first, second := plantedCheckout(t, ran)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty\n"), 0o644)
	for _, args := range [][]string{
		{"status", "--porcelain=v1", "-uall"},
		{"merge-base", "--is-ancestor", first, second},
		{"checkout", "-q", "-f", "--detach", first},
		{"update-index", "--refresh"},
		{"write-tree"},
		{"commit-tree", "-m", "x", second + "^{tree}"},
	} {
		cmd := Command(context.Background(), dir, []string{"GIT_AUTHOR_NAME=n", "GIT_AUTHOR_EMAIL=n@x", "GIT_COMMITTER_NAME=n", "GIT_COMMITTER_EMAIL=n@x"}, args...)
		if out, err := cmd.CombinedOutput(); err != nil && args[0] != "update-index" {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if left, _ := os.ReadDir(ran); len(left) != 0 {
		var names []string
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Fatalf("the checkout's planted commands ran: %v", names)
	}
}

// The plant is real: the same checkout run by git unhardened runs the
// session's commands, which is what the hardening stops.
func TestThePlantRunsUnhardened(t *testing.T) {
	ran := t.TempDir()
	dir, first, _ := plantedCheckout(t, ran)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty\n"), 0o644)
	plainGit(t, dir, "status", "--porcelain")
	plainGit(t, dir, "checkout", "-q", "-f", "--detach", first)
	left, _ := os.ReadDir(ran)
	if len(left) == 0 {
		t.Fatal("the planted config ran nothing unhardened; the hardening test proves nothing")
	}
}

// Every command driver the checkout's config names is cleared, and
// nothing else.
func TestDriverOverridesClearEachDriver(t *testing.T) {
	ran := t.TempDir()
	dir, _, _ := plantedCheckout(t, ran)
	plainGit(t, dir, "config", "remote.origin.uploadpack", "touch /nope")
	plainGit(t, dir, "config", "user.name", "kept")
	got, err := driverOverrides(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"filter.planted.clean=", "filter.planted.smudge=", "filter.planted.process=", "merge.planted.driver=",
		"diff.planted.textconv=", "credential.https://example.com.helper=", "remote.origin.uploadpack="} {
		if !slices.Contains(got, want) {
			t.Errorf("no override %q in %v", want, got)
		}
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "user.") || strings.HasPrefix(kv, "filter.planted.required") {
			t.Errorf("an override of a key that runs nothing: %q", kv)
		}
	}
}

// The environment drops the node's credentials and caller config and
// reads no system or global config.
func TestEnvDropsCredentialsAndOuterConfig(t *testing.T) {
	env := Env([]string{"PATH=/bin", "GH_TOKEN=t", "GIT_ASKPASS=/a", "GIT_CONFIG_PARAMETERS='core.fsmonitor'='x'",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=x", "GIT_SSH_COMMAND=x"})
	want := []string{"PATH=/bin", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	if !slices.Equal(env, want) {
		t.Fatalf("env %v, want %v", env, want)
	}
}

// A checkout whose config git cannot read gets a command that fails to
// start, never one run on config the node did not read.
func TestAnUnreadableConfigFailsClosed(t *testing.T) {
	dir := t.TempDir()
	plainGit(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[core\nbroken"), 0o644)
	if err := Command(context.Background(), dir, nil, "status").Run(); err == nil || !strings.Contains(err.Error(), "reading the checkout's git config") {
		t.Fatalf("a broken config ran: %v", err)
	}
}
