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
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

// Prepare on a clone whose branch exists locally but not on origin — an
// attempt interrupted before its push — continues that branch instead
// of resetting it onto the base.
func TestPrepareKeepsAnUnpushedLocalBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	git := func(dir string, args ...string) string {
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
	git(root, "init", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	git(root, "clone", "-q", origin, seed)
	os.WriteFile(filepath.Join(seed, "a"), []byte("a"), 0o644)
	git(seed, "add", "a")
	git(seed, "commit", "-q", "-m", "a")
	git(seed, "push", "-q", "origin", "HEAD:main")

	// the workspace: a clone with an unpushed commit on the item's branch
	spec := WorkspaceSpec{Root: root, GoalSlug: "g", Seq: 1, Repo: origin, Branch: "mesh/g-1", BaseRef: "main"}
	dir := spec.Dir()
	git(root, "clone", "-q", origin, dir)
	git(dir, "checkout", "-q", "-b", "mesh/g-1")
	os.WriteFile(filepath.Join(dir, "b"), []byte("b"), 0o644)
	git(dir, "add", "b")
	git(dir, "commit", "-q", "-m", "b")
	local := git(dir, "rev-parse", "HEAD")

	g := &Git{}
	got, err := g.Prepare(context.Background(), spec)
	if err != nil || got != dir {
		t.Fatalf("prepare: %v %s", err, got)
	}
	if head := git(dir, "rev-parse", "HEAD"); head != local {
		t.Fatalf("the local branch was reset: %s != %s", head, local)
	}
	if branch := git(dir, "branch", "--show-current"); branch != "mesh/g-1" {
		t.Fatalf("branch = %s", branch)
	}
}

// A checkout authorized with an item's token pushes to its own repo and
// cannot push to a second one — though the operator's own credential
// helper, still in their global git config, would reach it.
func TestAuthorizedWorkspaceReachesOnlyItsRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	// the operator's global git config: a helper that reaches every repo
	global := filepath.Join(root, "gitconfig")
	os.WriteFile(global, []byte("[credential]\n\thelper = \"!f() { echo username=operator; echo password=operator-login; }; f\"\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, kv := range []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_TERMINAL_PROMPT=0"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	git := func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(dir string, args ...string) string {
		t.Helper()
		out, err := git(dir, args...)
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return out
	}
	srvRoot := filepath.Join(root, "srv")
	for _, repo := range []string{"acme/widgets", "other/gadgets"} {
		bare := filepath.Join(srvRoot, repo+".git")
		must(root, "init", "--quiet", "--bare", "-b", "main", bare)
		seed := filepath.Join(root, "seed-"+strings.ReplaceAll(repo, "/", "-"))
		must(root, "init", "--quiet", "-b", "main", seed)
		os.WriteFile(filepath.Join(seed, "a"), []byte(repo), 0o644)
		must(seed, "add", "a")
		must(seed, "commit", "--quiet", "-m", "a")
		must(seed, "push", "--quiet", bare, "HEAD:main")
	}
	srv := gatewaytest.GitHub(t, srvRoot, map[string]string{"ghs_item": "acme/widgets", "operator-login": "*"})

	// the stand-in is honest: the operator's login reaches the second repo
	if out, err := git(root, "ls-remote", srv.URL+"/other/gadgets.git"); err != nil {
		t.Fatalf("the operator's helper should reach other/gadgets: %v: %s", err, out)
	}

	g := &Git{GitHubURL: srv.URL}
	spec := WorkspaceSpec{Root: filepath.Join(root, "work"), GoalSlug: "g", Seq: 1, Repo: "acme/widgets", Branch: "mesh/g-1", BaseRef: "main"}
	tok := &gateway.GitHubToken{Token: "ghs_item", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets"}
	ctx := context.Background()
	if err := g.Authorize(ctx, spec, tok); err != nil {
		t.Fatal(err)
	}
	dir, err := g.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("prepare on the item's token: %v", err)
	}
	// the credential lives in the checkout, not the operator's config
	if cfg, _ := os.ReadFile(global); strings.Contains(string(cfg), "ghs_item") {
		t.Fatal("the token leaked into the global git config")
	}
	os.WriteFile(filepath.Join(dir, "b"), []byte("b"), 0o644)
	must(dir, "add", "b")
	must(dir, "commit", "--quiet", "-m", "b")
	sha, err := g.Push(ctx, dir, spec.Branch)
	if err != nil {
		t.Fatalf("push to the item's own repo: %v", err)
	}
	if got := must(root, "--git-dir", filepath.Join(srvRoot, "acme/widgets.git"), "rev-parse", spec.Branch); got != sha {
		t.Fatalf("pushed %s, origin has %s", sha, got)
	}

	// a session pushing to a second repo from inside the checkout fails
	if out, err := git(dir, "push", srv.URL+"/other/gadgets.git", "HEAD:refs/heads/stolen"); err == nil {
		t.Fatalf("a push to a second repo must fail: %s", out)
	}
	must(dir, "remote", "add", "second", srv.URL+"/other/gadgets.git")
	if out, err := git(dir, "push", "second", "HEAD:refs/heads/stolen"); err == nil {
		t.Fatalf("a push to a second remote must fail: %s", out)
	}
	if _, err := git(root, "--git-dir", filepath.Join(srvRoot, "other/gadgets.git"), "rev-parse", "--verify", "stolen"); err == nil {
		t.Fatal("the second repo took the push")
	}

	// a renewed token replaces the old one in the checkout
	g2 := &gateway.GitHubToken{Token: "ghs_renewed", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets"}
	if err := g.Authorize(ctx, spec, g2); err != nil {
		t.Fatal(err)
	}
	helpers := must(dir, "config", "--local", "--get-all", "credential."+srv.URL+".helper")
	if strings.Contains(helpers, "ghs_item") || !strings.Contains(helpers, "ghs_renewed") {
		t.Fatalf("helpers after renewal:\n%s", helpers)
	}
}

// Fetch gives the planner's read-only checkout the default branch's
// recent history, not one commit: its `git log` is how a replan sees
// what main already holds (adr-003 decision 2).
func TestFetchCarriesBaseRefHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	git := func(dir string, args ...string) string {
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
	git(root, "init", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	git(root, "clone", "-q", origin, seed)
	commit := func(msg string) {
		os.WriteFile(filepath.Join(seed, "f"), []byte(msg), 0o644)
		git(seed, "add", "f")
		git(seed, "commit", "-q", "-m", msg)
		git(seed, "push", "-q", "origin", "HEAD:main")
	}
	commit("first")
	// the plan checkout as an older node left it: one commit deep
	dir := filepath.Join(root, "plan", "repos", "widgets")
	git(root, "clone", "-q", "--depth", "1", "file://"+origin, dir)
	for _, msg := range []string{"item 1 merged", "item 2 merged", "item 3 merged"} {
		commit(msg)
	}

	if err := (&Git{}).Fetch(context.Background(), "acme/widgets", dir); err != nil {
		t.Fatal(err)
	}
	if n := git(dir, "rev-list", "--count", "HEAD"); n != "4" {
		t.Fatalf("history after fetch = %s commits, want 4", n)
	}
	if log := git(dir, "log", "--format=%s"); log != "item 3 merged\nitem 2 merged\nitem 1 merged\nfirst" {
		t.Fatalf("log:\n%s", log)
	}
}

// PushedHead reads back what origin has for the branch after a fetch,
// and refuses a branch that does not contain the base: a branch pushed
// without the rebase, or not pushed at all.
func TestPushedHeadReadsOriginBack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	git := func(dir string, args ...string) string {
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
	commit := func(dir, name string) {
		os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644)
		git(dir, "add", name)
		git(dir, "commit", "-q", "-m", name)
	}
	git(root, "init", "--bare", "-b", "main", origin)
	dir := filepath.Join(root, "ws")
	git(root, "clone", "-q", origin, dir)
	commit(dir, "a")
	git(dir, "push", "-q", "origin", "HEAD:main")
	git(dir, "checkout", "-q", "-b", "mesh/g-1")
	commit(dir, "b")
	git(dir, "push", "-q", "origin", "mesh/g-1")
	// main moves on: the branch is behind it
	other := filepath.Join(root, "other")
	git(root, "clone", "-q", origin, other)
	commit(other, "c")
	git(other, "push", "-q", "origin", "HEAD:main")

	g := &Git{}
	ctx := context.Background()
	if _, err := g.PushedHead(ctx, dir, "mesh/g-1", "main"); err == nil || !strings.Contains(err.Error(), "does not contain origin/main") {
		t.Fatalf("a branch not rebased: %v", err)
	}
	if _, err := g.PushedHead(ctx, dir, "mesh/g-2", "main"); err == nil {
		t.Fatal("a branch origin does not have")
	}
	git(dir, "fetch", "-q", "origin")
	git(dir, "rebase", "-q", "origin/main")
	git(dir, "push", "-q", "--force-with-lease", "origin", "mesh/g-1")
	want := git(dir, "rev-parse", "HEAD")
	git(dir, "reset", "-q", "--hard", "HEAD~1") // the local checkout is not what is read
	if head, err := g.PushedHead(ctx, dir, "mesh/g-1", "main"); err != nil || head != want {
		t.Fatalf("pushed head = %s, %v; want %s", head, err, want)
	}
}
