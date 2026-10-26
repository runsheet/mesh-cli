package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// plainGit runs git as a test does, with its own identity: never the
// hardened runner.
func plainGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// markerCommand is a shell command that writes marker when anything runs it.
func markerCommand(t *testing.T, root, marker string) string {
	t.Helper()
	script := filepath.Join(root, "write-"+marker+".sh")
	body := "#!/bin/sh\necho ran >> '" + filepath.Join(root, marker) + "'\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

var tamperMarkers = []string{"fsmonitor", "hookspath", "credential", "sshcommand", "prepush"}

// tamper writes what a session in the pod could into dir's .git: each
// key, and a .git/hooks/pre-push, a command that writes a marker.
func tamper(t *testing.T, root, dir string) {
	t.Helper()
	hooks := filepath.Join(root, "evil-hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"pre-push", "post-checkout", "reference-transaction", "pre-commit"} {
		os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\necho ran >> '"+filepath.Join(root, "hookspath")+"'\n"), 0o755)
	}
	plainGit(t, dir, "config", "core.fsmonitor", markerCommand(t, root, "fsmonitor"))
	plainGit(t, dir, "config", "core.hooksPath", hooks)
	plainGit(t, dir, "config", "credential.helper", "!"+markerCommand(t, root, "credential"))
	plainGit(t, dir, "config", "core.sshCommand", markerCommand(t, root, "sshcommand"))
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-push"), []byte("#!/bin/sh\necho ran >> '"+filepath.Join(root, "prepush")+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func markersWritten(root string) []string {
	var got []string
	for _, m := range tamperMarkers {
		if _, err := os.Stat(filepath.Join(root, m)); err == nil {
			got = append(got, m)
		}
	}
	return got
}

// A session can write .git/config and .git/hooks in its pod; the node's
// diff, fetch and push on the host after it run none of what it wrote,
// and the push still lands.
func TestHardenedGitIgnoresTamperedWorkspace(t *testing.T) {
	mustLookPath(t, "git")
	root := t.TempDir()
	// the remote by GitHubURL, a file:// one, so the checkout is a
	// clone of its own on the item's token, as a job's is (ADR-039)
	origin := filepath.Join(root, "acme", "widgets.git")
	os.MkdirAll(filepath.Dir(origin), 0o755)
	plainGit(t, root, "init", "--quiet", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	plainGit(t, root, "clone", "-q", origin, seed)
	os.WriteFile(filepath.Join(seed, "a"), []byte("a"), 0o644)
	plainGit(t, seed, "add", "a")
	plainGit(t, seed, "commit", "-q", "-m", "a")
	plainGit(t, seed, "push", "-q", "origin", "HEAD:main")

	job := filepath.Join(root, "jobs", "item")
	spec := WorkspaceSpec{Root: job, GoalSlug: "g", Seq: 1, Repo: "acme/widgets", Branch: "mesh/g-1", BaseRef: "main", Job: job}
	g := &Git{GitHubURL: "file://" + root}
	tok := &gateway.GitHubToken{Token: "ghs_item", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets"}
	ctx := OnItemToken(context.Background(), g, tok)
	dir, err := g.Prepare(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "b"), []byte("b"), 0o644)
	plainGit(t, dir, "add", "b")
	plainGit(t, dir, "commit", "-q", "-m", "b")
	tamper(t, root, dir)

	// the control: plain git runs what was written
	plainGit(t, dir, "status")
	if got := markersWritten(root); len(got) == 0 {
		t.Fatal("the tampered config ran nothing under plain git; the test proves nothing")
	}
	for _, m := range tamperMarkers {
		os.Remove(filepath.Join(root, m))
	}

	if _, err := g.Diff(ctx, dir, "main"); err != nil {
		t.Fatalf("diff: %v", err)
	}
	if _, err := g.ChangedPaths(ctx, dir, "main"); err != nil {
		t.Fatalf("changed paths: %v", err)
	}
	if _, err := g.run(ctx, dir, "git", "status"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := g.Fetch(ctx, "acme/widgets", dir); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	sha, err := g.Push(ctx, dir, "mesh/g-1")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := markersWritten(root); len(got) != 0 {
		t.Fatalf("the node ran what the session wrote: %v", got)
	}
	if remote := plainGit(t, origin, "rev-parse", "refs/heads/mesh/g-1"); remote != sha {
		t.Fatalf("origin's mesh/g-1 = %s, pushed %s", remote, sha)
	}
}

// The node's own hooks dir, when the checkout has one, is the hooks
// path: the node's prepare-commit-msg still runs, a session's hooks
// never.
func TestHardenedGitHooksPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkout")
	if got := hardenedGitArgs(dir); !containsArg(got, "core.hooksPath="+os.DevNull) {
		t.Fatalf("no hooks dir: %v", got)
	}
	if err := os.MkdirAll(HooksDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := hardenedGitArgs(dir); !containsArg(got, "core.hooksPath="+HooksDir(dir)) {
		t.Fatalf("hooks dir: %v", got)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// The git child sees none of the operator's GitHub or git environment.
func TestHardenedGitEnvironment(t *testing.T) {
	mustLookPath(t, "git")
	dir := t.TempDir()
	plainGit(t, dir, "init", "--quiet")
	t.Setenv("GH_TOKEN", "ghp_operator")
	t.Setenv("GH_CONFIG_DIR", filepath.Join(dir, "gh"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	t.Setenv("GIT_SSH_COMMAND", "touch "+filepath.Join(dir, "ssh"))
	t.Setenv("GIT_DIR", filepath.Join(dir, "elsewhere"))
	cmd := hardenedGitCmd(context.Background(), "", dir, "-c", "alias.envdump=!env", "envdump")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("env: %v: %s", err, out)
	}
	env := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			env[k] = v
		}
	}
	for _, k := range []string{"GH_TOKEN", "GH_CONFIG_DIR", "GIT_CONFIG_GLOBAL", "GIT_SSH_COMMAND"} {
		if v, ok := env[k]; ok {
			t.Errorf("git's child has %s=%s", k, v)
		}
	}
	if env["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Errorf("GIT_CONFIG_NOSYSTEM = %q", env["GIT_CONFIG_NOSYSTEM"])
	}
	if env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q", env["GIT_TERMINAL_PROMPT"])
	}
	if strings.Contains(string(out), "ghp_operator") {
		t.Error("the operator's token reached git's child")
	}
}

// The reset of the helpers comes before the item token's, so a command
// on OnItemToken still answers with the token — and a helper the
// session wrote into .git/config never runs.
func TestHardenedGitKeepsItemTokenHelper(t *testing.T) {
	mustLookPath(t, "git")
	root := t.TempDir()
	dir := filepath.Join(root, "checkout")
	plainGit(t, root, "init", "--quiet", dir)
	plainGit(t, dir, "config", "credential.helper", "!"+markerCommand(t, root, "credential"))
	fakeOperatorGH(t, root, "gho_operator")
	g := &Git{GitHubURL: "https://github.com"}
	tok := &gateway.GitHubToken{Token: "ghs_item", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets"}
	fill := func(ctx context.Context) string {
		t.Helper()
		cmd := hardenedGitCmd(ctx, g.githubURL(), dir, "credential", "fill")
		cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=acme/widgets.git\n\n")
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	if out := fill(OnItemToken(context.Background(), g, tok)); !strings.Contains(out, "password=ghs_item") {
		t.Fatalf("credential fill on the item token:\n%s", out)
	}
	if out := fill(withGitToken(context.Background(), g.githubURL(), tok)); !strings.Contains(out, "password=ghs_item") {
		t.Fatalf("credential fill on withGitToken:\n%s", out)
	}
	// with no token, the operator's gh answers: not the session's helper
	if out := fill(context.Background()); !strings.Contains(out, "password=gho_operator") {
		t.Fatalf("credential fill with no token:\n%s", out)
	}
	if got := markersWritten(root); len(got) != 0 {
		t.Fatalf("the session's helper ran: %v", got)
	}
	// the helper the node wrote answers with no token on ctx, though the
	// session rewrote that very key in .git/config
	if err := WriteCredentialHelper(context.Background(), dir, g.githubURL(), tok); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { forgetHelper(dir) })
	plainGit(t, dir, "config", "--add", "credential."+g.githubURL()+".helper", "!"+markerCommand(t, root, "credential"))
	if out := fill(context.Background()); !strings.Contains(out, "password=ghs_item") {
		t.Fatalf("credential fill on the node's own helper:\n%s", out)
	}
	if got := markersWritten(root); len(got) != 0 {
		t.Fatalf("the session's helper ran: %v", got)
	}
}

// fakeOperatorGH puts a gh on PATH whose `auth git-credential get`
// answers password, recording the GH_CONFIG_DIR it ran with in
// <root>/gh-config-dir, and points the operator's gh config at
// <root>/operator-gh.
func fakeOperatorGH(t *testing.T, root, password string) string {
	t.Helper()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"[ \"$1 $2 $3\" = \"auth git-credential get\" ] || exit 0\n" +
		"printf '%s' \"$GH_CONFIG_DIR\" > '" + filepath.Join(root, "gh-config-dir") + "'\n" +
		"printf 'protocol=https\\nhost=github.com\\nusername=x-access-token\\npassword=" + password + "\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ghDir := filepath.Join(root, "operator-gh")
	t.Setenv("GH_CONFIG_DIR", ghDir)
	return ghDir
}

// A tokenless command — the old platform's fallback, a main validation
// request's fetch after `gh repo clone` — answers from the operator's
// own gh at its config dir, as the operator's global helper did before
// the reset; a helper the session wrote into .git/config, for the
// GitHub base or for every host, still never runs, nor does a global
// one the operator's ~/.gitconfig names.
func TestHardenedGitFallsBackToOperatorHelper(t *testing.T) {
	mustLookPath(t, "git")
	root := t.TempDir()
	dir := filepath.Join(root, "checkout")
	plainGit(t, root, "init", "--quiet", dir)
	ghDir := fakeOperatorGH(t, root, "gho_operator")
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GH_TOKEN", "ghp_env")
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[credential]\n\thelper = !"+markerCommand(t, root, "global")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &Git{GitHubURL: "https://github.com"}
	plainGit(t, dir, "config", "credential.helper", "!"+markerCommand(t, root, "credential"))
	plainGit(t, dir, "config", "--add", "credential."+g.githubURL()+".helper", "!"+markerCommand(t, root, "credential-base"))
	cmd := hardenedGitCmd(context.Background(), g.githubURL(), dir, "credential", "fill")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=acme/widgets.git\n\n")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "password=gho_operator") {
		t.Fatalf("tokenless credential fill: %v:\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "gh-config-dir")); string(got) != ghDir {
		t.Errorf("the operator's gh ran with GH_CONFIG_DIR %q, want %q", got, ghDir)
	}
	for _, m := range []string{"global", "credential", "credential-base"} {
		if _, err := os.Stat(filepath.Join(root, m)); err == nil {
			t.Errorf("the %s helper ran", m)
		}
	}
}
