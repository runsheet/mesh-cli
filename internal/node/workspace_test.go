package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// A plan's checkout Authorize made empty is fetched with git alone —
// no `gh repo clone`, so no gh login — and lands on origin's default
// branch, not whichever branch sorts first (adr-022).
func TestFetchIntoAnAuthorizedCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	// no gh anywhere: a clone through it would fail
	t.Setenv("PATH", filepath.Dir(mustLookPath(t, "git")))
	root := t.TempDir()
	origin := filepath.Join(root, "acme", "widgets.git")
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
	os.MkdirAll(filepath.Dir(origin), 0o755)
	git(root, "init", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	git(root, "clone", "-q", origin, seed)
	os.WriteFile(filepath.Join(seed, "f"), []byte("main"), 0o644)
	git(seed, "add", "f")
	git(seed, "commit", "-q", "-m", "on main")
	git(seed, "push", "-q", "origin", "HEAD:main")
	git(seed, "commit", "-q", "--allow-empty", "-m", "on a side branch")
	git(seed, "push", "-q", "origin", "HEAD:aaa-side")

	g := &Git{GitHubURL: "file://" + root}
	dir := filepath.Join(root, "plan", "repos", "widgets")
	tok := &gateway.GitHubToken{Token: "ghs_plan", ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets"}
	if err := g.Authorize(context.Background(), PlanRepoSpec("acme/widgets", dir), tok); err != nil {
		t.Fatal(err)
	}
	if err := g.Fetch(context.Background(), "acme/widgets", dir); err != nil {
		t.Fatal(err)
	}
	if log := git(dir, "log", "--format=%s"); log != "on main" {
		t.Fatalf("log after fetch:\n%s", log)
	}
	// and again, as the next plan of the goal refreshes it
	if err := g.Authorize(context.Background(), PlanRepoSpec("acme/widgets", dir), tok); err != nil {
		t.Fatal(err)
	}
	if err := g.Fetch(context.Background(), "acme/widgets", dir); err != nil {
		t.Fatal(err)
	}
	if log := git(dir, "log", "--format=%s"); log != "on main" {
		t.Fatalf("log after refetch:\n%s", log)
	}
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skip("no " + name)
	}
	return p
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

// mergeBaseFixture builds an origin whose main has 40 commits and clones it
// depth 1, single-branch; the item branch is pushed to origin, forked
// well back from main's tip (or an orphan), and checked out as Prepare does.
func mergeBaseFixture(t *testing.T, orphan bool) (dir string) {
	t.Helper()
	return mergeBaseFixtureBranch(t, orphan, "mesh/g-1")
}

// mergeBaseFixtureBranch is mergeBaseFixture with the item branch named.
func mergeBaseFixtureBranch(t *testing.T, orphan bool, branch string) (dir string) {
	t.Helper()
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
	git(root, "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	git(root, "clone", "-q", origin, seed)
	for i := 0; i < 40; i++ {
		os.WriteFile(filepath.Join(seed, "n"), []byte(strings.Repeat("x", i+1)), 0o644)
		git(seed, "add", "n")
		git(seed, "commit", "-q", "-m", "c")
		if i == 5 && !orphan {
			git(seed, "checkout", "-q", "-b", branch)
			os.WriteFile(filepath.Join(seed, "feature"), []byte("the branch's change\n"), 0o644)
			git(seed, "add", "feature")
			git(seed, "commit", "-q", "-m", "feature")
			git(seed, "checkout", "-q", "main")
		}
	}
	if orphan {
		git(seed, "checkout", "-q", "--orphan", branch)
		os.WriteFile(filepath.Join(seed, "feature"), []byte("the branch's change\n"), 0o644)
		git(seed, "add", "feature")
		git(seed, "commit", "-q", "-m", "feature")
		git(seed, "checkout", "-q", "main")
	}
	git(seed, "push", "-q", "origin", "main", branch)

	dir = filepath.Join(root, "ws")
	git(root, "clone", "-q", "--no-local", "--depth", "1", "--single-branch", "--branch", "main", "file://"+origin, dir)
	git(dir, "fetch", "-q", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	git(dir, "checkout", "-q", "-B", branch, "origin/"+branch)
	return dir
}

func TestDiffDeepensAShallowCheckoutToTheMergeBase(t *testing.T) {
	dir := mergeBaseFixture(t, false)
	out, err := (&Git{}).Diff(context.Background(), dir, "main")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(out, "feature") || !strings.Contains(out, "the branch's change") {
		t.Fatalf("diff lacks the branch's change:\n%s", out)
	}
}

func TestDiffWithNoMergeBaseIsTheItemsProblem(t *testing.T) {
	dir := mergeBaseFixture(t, true)
	_, err := (&Git{}).Diff(context.Background(), dir, "main")
	var nm *NoMergeBaseError
	if !errors.As(err, &nm) {
		t.Fatalf("err = %v, want *NoMergeBaseError", err)
	}
	if nm.Base != "main" || nm.Branch != "mesh/g-1" || !strings.Contains(err.Error(), "shares no history with origin/main") {
		t.Fatalf("err = %v", err)
	}
}

// An already_done item's evaluation has no branch: Prepare's base
// checkout leaves a shallow single-branch clone on origin/main,
// detached, with main's whole history to look a cited SHA up in.
func TestCheckoutBaseHasTheHistory(t *testing.T) {
	dir := mergeBaseFixtureBranch(t, false, "mesh/ship-it-1")
	first, err := exec.Command("git", "-C", filepath.Join(filepath.Dir(dir), "seed"), "rev-list", "--max-parents=0", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Git{}).CheckoutBase(context.Background(), dir, "main"); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := git("rev-parse", "--is-shallow-repository"); got != "false" {
		t.Fatalf("still shallow: %s", got)
	}
	if git("rev-parse", "HEAD") != git("rev-parse", "origin/main") || git("rev-parse", "--abbrev-ref", "HEAD") != "HEAD" {
		t.Fatal("not on origin/main, detached")
	}
	if err := exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", strings.TrimSpace(string(first)), "origin/main").Run(); err != nil {
		t.Fatalf("main's first commit is not reachable: %v", err)
	}
}

// Prepare on a shallow, single-branch checkout gives the build session
// the base ref's history (adr-003 decision 2: the builder checks main
// first with `git log origin/<base>`).
func TestPrepareGivesTheBaseRefsHistory(t *testing.T) {
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
	git(root, "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	git(root, "clone", "-q", origin, seed)
	for i := 1; i <= 12; i++ {
		os.WriteFile(filepath.Join(seed, "f"), []byte(strings.Repeat("x", i)), 0o644)
		git(seed, "add", "f")
		git(seed, "commit", "-q", "-m", "item "+strconv.Itoa(i)+" merged")
	}
	git(seed, "push", "-q", "origin", "main")
	git(seed, "checkout", "-q", "-b", "other")
	git(seed, "push", "-q", "origin", "other")

	// the item's checkout, one commit deep on another branch alone
	spec := WorkspaceSpec{Root: root, GoalSlug: "g", Seq: 7, Repo: origin, Branch: "mesh/g-7", BaseRef: "main"}
	dir := spec.Dir()
	git(root, "clone", "-q", "--no-local", "--depth", "1", "--single-branch", "--branch", "other", "file://"+origin, dir)
	if _, err := (&Git{}).Prepare(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if n := git(dir, "rev-list", "--count", "origin/main"); n != "12" {
		t.Fatalf("origin/main history = %s commits, want 12", n)
	}
	if log := git(dir, "log", "--format=%s", "origin/main"); !strings.Contains(log, "item 3 merged") || !strings.Contains(log, "item 1 merged") {
		t.Fatalf("log origin/main:\n%s", log)
	}
	if branch := git(dir, "branch", "--show-current"); branch != "mesh/g-7" {
		t.Fatalf("branch = %s", branch)
	}
}

// Push against a branch that moved on origin since the checkout last
// saw it is a *PushRefusedError naming --force-with-lease, and origin
// keeps its head; a remote that cannot be reached is not one.
func TestPushRefusedWhenTheRemoteMoved(t *testing.T) {
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
	seed := filepath.Join(root, "seed")
	git(root, "clone", "-q", origin, seed)
	commit(seed, "a")
	git(seed, "push", "-q", "origin", "HEAD:main", "HEAD:refs/heads/mesh/g-1")

	holder := filepath.Join(root, "holder")
	pair := filepath.Join(root, "pair")
	for _, d := range []string{holder, pair} {
		git(root, "clone", "-q", "-b", "mesh/g-1", origin, d)
	}
	commit(holder, "b")
	git(holder, "push", "-q", "origin", "mesh/g-1")
	moved := git(origin, "rev-parse", "refs/heads/mesh/g-1")
	commit(pair, "c")

	g := &Git{}
	_, err := g.Push(context.Background(), pair, "mesh/g-1")
	var refused *PushRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("push on a moved remote: %v (%T)", err, err)
	}
	if !strings.Contains(err.Error(), "--force-with-lease") || refused.Branch != "mesh/g-1" || !PushRefusal(refused.Output) {
		t.Fatalf("refusal: %v", err)
	}
	if got := git(origin, "rev-parse", "refs/heads/mesh/g-1"); got != moved {
		t.Fatalf("origin moved to %s, want %s", got, moved)
	}

	git(pair, "remote", "set-url", "origin", filepath.Join(root, "gone.git"))
	if _, err := g.Push(context.Background(), pair, "mesh/g-1"); err == nil || errors.As(err, &refused) {
		t.Fatalf("an unreachable remote: %v", err)
	}
}

// lockTestOrigin is a bare origin with a seeded main under a fresh root,
// and a `gh` on PATH that clones it with git: `gh repo clone` takes no
// local path. It returns the root and the origin.
func lockTestOrigin(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	git(root, "init", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	git(root, "clone", "-q", origin, seed)
	for i := 0; i < 3; i++ {
		os.WriteFile(filepath.Join(seed, "a"), []byte(strconv.Itoa(i)), 0o644)
		git(seed, "add", "a")
		git(seed, "commit", "-q", "-m", "a"+strconv.Itoa(i))
	}
	git(seed, "push", "-q", "origin", "HEAD:main")
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	gh := "#!/bin/sh\n# gh repo clone <repo> <dir> [-- <git clone flags>]\nshift 2\nrepo=$1; dir=$2; shift 2\n[ \"$1\" = \"--\" ] && shift\nexec git clone -q \"$@\" \"$repo\" \"$dir\"\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return root, origin
}

// prepareAtOnce runs Prepare for every spec at the same instant and
// returns each one's error.
func prepareAtOnce(specs []WorkspaceSpec) []error {
	errs := make([]error, len(specs))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec WorkspaceSpec) {
			defer wg.Done()
			<-start
			_, errs[i] = (&Git{}).Prepare(context.Background(), spec)
		}(i, spec)
	}
	close(start)
	wg.Wait()
	return errs
}

// Four items of one repo prepared at once — the grapevine node's four
// claims within seconds, 2026-10-03 — all succeed, and none fails on a
// ref another fetch was writing: their fetches run under the repo's lock.
func TestPrepareFourItemsOfOneRepoAtOnce(t *testing.T) {
	root, origin := lockTestOrigin(t)
	var specs []WorkspaceSpec
	for seq := 1; seq <= 4; seq++ {
		specs = append(specs, WorkspaceSpec{Root: root, GoalSlug: "g", Seq: seq, Repo: origin, Branch: BranchName("g", seq), BaseRef: "main"})
	}
	for round := 0; round < 2; round++ { // a clone each, then a fetch each
		for i, err := range prepareAtOnce(specs) {
			if err != nil {
				t.Fatalf("round %d: prepare seq %d: %v", round, i+1, err)
			}
		}
	}
	for _, spec := range specs {
		if _, err := os.Stat(filepath.Join(spec.Dir(), ".git")); err != nil {
			t.Fatalf("seq %d: no clone of its own: %v", spec.Seq, err)
		}
	}
}

// Four prepares of the one spec at once — one checkout, every fetch
// into the same refs — never fail on `cannot lock ref`: any error left
// is the checkout's own, not a remote-tracking ref two fetches wrote.
func TestPrepareOneSpecFourTimesAtOnce(t *testing.T) {
	root, origin := lockTestOrigin(t)
	spec := WorkspaceSpec{Root: root, GoalSlug: "g", Seq: 1, Repo: origin, Branch: "mesh/g-1", BaseRef: "main"}
	if _, err := (&Git{}).Prepare(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	// origin's main moves, so every fetch has refs/remotes/origin/main to write
	seed := filepath.Join(root, "seed")
	for _, args := range [][]string{{"commit", "-q", "--allow-empty", "-m", "moved"}, {"push", "-q", "origin", "HEAD:main"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = seed
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	specs := []WorkspaceSpec{spec, spec, spec, spec}
	for i, err := range prepareAtOnce(specs) {
		if err != nil && strings.Contains(err.Error(), "cannot lock ref") {
			t.Fatalf("prepare %d failed on a ref lock: %v", i, err)
		}
		if err != nil {
			t.Logf("prepare %d: %v", i, err)
		}
	}
}

// Prepare leaves the repo's lock file under <root>/.locks/ and lets it
// go: a non-blocking exclusive flock on it succeeds once Prepare returns.
func TestPrepareReleasesTheRepoLock(t *testing.T) {
	root, origin := lockTestOrigin(t)
	spec := WorkspaceSpec{Root: root, GoalSlug: "g", Seq: 1, Repo: origin, Branch: "mesh/g-1", BaseRef: "main"}
	if _, err := (&Git{}).Prepare(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	path := RepoLockPath(root, RepoLockKey(origin))
	if filepath.Dir(path) != filepath.Join(root, LocksDir) {
		t.Fatalf("lock file %s is not under %s", path, filepath.Join(root, LocksDir))
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("no lock file after Prepare: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("the repo lock is still held after Prepare: %v", err)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// The lock key is owner/name however the repo is spelt, so a clone by
// owner/name and a fetch read back from origin's URL share one lock.
func TestRepoLockKey(t *testing.T) {
	for _, in := range []string{"Runsheet/Mesh-CLI", "https://github.com/runsheet/mesh-cli.git", "git@github.com:runsheet/mesh-cli.git", "https://github.com/runsheet/mesh-cli"} {
		if got := RepoLockKey(in); got != "runsheet/mesh-cli" {
			t.Errorf("RepoLockKey(%q) = %q", in, got)
		}
	}
	if got := RepoLockPath("/w", "runsheet/mesh-cli"); got != "/w/.locks/runsheet--mesh-cli.lock" {
		t.Errorf("RepoLockPath = %s", got)
	}
	if got := RepoLockKey("/tmp/x/origin.git"); got != "/tmp/x/origin" {
		t.Errorf("a path keys as itself: %s", got)
	}
}

// IsRefLockFailure matches git refusing a ref update because another
// process was writing the same ref at the same moment — the two lines
// the grapevine node's log carried, 2026-10-03, before fetches of one
// repo were serialised on the node (ADR-023) — and nothing else.
func TestIsRefLockFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			"item 1's cannot lock ref",
			errors.New("workspace: git fetch --quiet origin +refs/heads/main:refs/remotes/origin/main: exit status 1: error: cannot lock ref 'refs/remotes/origin/main': is at d843d265…"),
			true,
		},
		{
			"item 2's unable to update local ref",
			errors.New("git fetch --prune origin: … error: cannot lock ref 'refs/remotes/origin/main': is at c67b5444… but expected d843d265… (unable to update local ref)"),
			true,
		},
		{"not found", errors.New("gh repo clone runsheet/mesh-cli: exit status 1: Repository not found"), false},
	}
	for _, c := range cases {
		if got := IsRefLockFailure(c.err); got != c.want {
			t.Errorf("%s: IsRefLockFailure() = %v, want %v", c.name, got, c.want)
		}
	}
}

// refLockBackoffError is the ref-lock failure text the brief gives, the
// shape fetchLocked retries on.
const refLockBackoffError = "git fetch --quiet origin: exit status 1: error: cannot lock ref 'refs/remotes/origin/main': is at d843d265 but expected c67b4444 (unable to update local ref)"

// noRefLockSleep makes refLockBackoff instant for a test, restored after.
func noRefLockSleep(t *testing.T) {
	t.Helper()
	prev := refLockSleep
	refLockSleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { refLockSleep = prev })
}

// refLockLogLines is every line of log containing "git refused a ref
// lock".
func refLockLogLines(log []string) []string {
	var out []string
	for _, l := range log {
		if strings.Contains(l, "git refused a ref lock") {
			out = append(out, l)
		}
	}
	return out
}

// A fetch refused twice with `cannot lock ref` and succeeding the third
// try is not a failed Prepare: the retry is inside the one fetch helper,
// logged once, naming the try it succeeded on.
func TestFetchRetriesARefLockFailureAndSucceeds(t *testing.T) {
	root, origin := lockTestOrigin(t)
	spec := WorkspaceSpec{Root: root, GoalSlug: "g", Seq: 1, Repo: origin, Branch: "mesh/g-1", BaseRef: "main"}
	if _, err := (&Git{}).Prepare(context.Background(), spec); err != nil {
		t.Fatalf("seed prepare: %v", err)
	}
	noRefLockSleep(t)

	var log []string
	g := &Git{Logf: func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) }}
	pruneCalls := 0
	g.exec = func(ctx context.Context, dir, name string, args ...string) (string, error) {
		if name == "git" && len(args) >= 2 && args[0] == "fetch" && args[1] == "--prune" {
			pruneCalls++
			if pruneCalls <= 2 {
				return "", errors.New(refLockBackoffError)
			}
		}
		return g.realRun(ctx, dir, name, args...)
	}
	if _, err := g.Prepare(context.Background(), spec); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if pruneCalls != 3 {
		t.Fatalf("the fetch ran %d times, want 3", pruneCalls)
	}
	lines := refLockLogLines(log)
	if len(lines) != 1 {
		t.Fatalf("log lines with 'git refused a ref lock' = %v, want exactly 1", lines)
	}
	if !strings.Contains(lines[0], "try 3") {
		t.Fatalf("log line = %q, want it to name try 3", lines[0])
	}
}

// A fetch refused three times with `cannot lock ref` gives up: Prepare
// fails with an error IsRefLockFailure reports true, and the log names
// giving up after three tries, once.
func TestFetchGivesUpAfterThreeRefLockFailures(t *testing.T) {
	root, origin := lockTestOrigin(t)
	spec := WorkspaceSpec{Root: root, GoalSlug: "g", Seq: 1, Repo: origin, Branch: "mesh/g-1", BaseRef: "main"}
	if _, err := (&Git{}).Prepare(context.Background(), spec); err != nil {
		t.Fatalf("seed prepare: %v", err)
	}
	noRefLockSleep(t)

	var log []string
	g := &Git{Logf: func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) }}
	pruneCalls := 0
	g.exec = func(ctx context.Context, dir, name string, args ...string) (string, error) {
		if name == "git" && len(args) >= 2 && args[0] == "fetch" && args[1] == "--prune" {
			pruneCalls++
			return "", errors.New(refLockBackoffError)
		}
		return g.realRun(ctx, dir, name, args...)
	}
	_, err := g.Prepare(context.Background(), spec)
	if err == nil {
		t.Fatal("want an error")
	}
	if !IsRefLockFailure(err) {
		t.Fatalf("err = %v, want IsRefLockFailure", err)
	}
	if pruneCalls != 3 {
		t.Fatalf("the fetch ran %d times, want 3", pruneCalls)
	}
	lines := refLockLogLines(log)
	if len(lines) != 1 {
		t.Fatalf("log lines with 'git refused a ref lock' = %v, want exactly 1", lines)
	}
	if !strings.Contains(lines[0], "gave up after 3 tries") {
		t.Fatalf("log line = %q, want it to say giving up after 3 tries", lines[0])
	}
}

// A non-lock error is returned at once, with no retry: the fetch helper
// runs exactly once.
func TestFetchReturnsANonLockErrorAtOnce(t *testing.T) {
	root, origin := lockTestOrigin(t)
	spec := WorkspaceSpec{Root: root, GoalSlug: "g", Seq: 1, Repo: origin, Branch: "mesh/g-1", BaseRef: "main"}
	calls := 0
	g := &Git{}
	g.exec = func(ctx context.Context, dir, name string, args ...string) (string, error) {
		calls++
		return "", errors.New("gh repo clone " + origin + ": exit status 1: Repository not found")
	}
	_, err := g.Prepare(context.Background(), spec)
	if err == nil {
		t.Fatal("want an error")
	}
	if IsRefLockFailure(err) {
		t.Fatalf("err = %v, want it not to be a ref-lock failure", err)
	}
	if calls != 1 {
		t.Fatalf("exec called %d times, want 1", calls)
	}
}

// The evaluation's checkout is not the build's: for the same goal, seq
// and repo the two specs name two directories, and only the eval's
// moves (the build, rework and rebase keep <root>/<goal>/<seq>).
func TestEvalWorkspaceSpecIsNotTheBuilds(t *testing.T) {
	root := t.TempDir()
	build := WorkspaceSpec{Root: root, GoalSlug: "ship-it", Seq: 3, Repo: "acme/widgets", Branch: "mesh/ship-it-3", BaseRef: "main"}
	eval := EvalWorkspaceSpec(root, "ship-it", 3, "acme/widgets", "mesh/ship-it-3", "main")
	if eval.Dir() == build.Dir() {
		t.Fatalf("eval dir %s is the build's", eval.Dir())
	}
	if build.Dir() != filepath.Join(root, "ship-it", "3") || eval.Dir() != filepath.Join(root, "ship-it", "3-eval") {
		t.Fatalf("build %s, eval %s", build.Dir(), eval.Dir())
	}
	if eval.Root != root || eval.Repo != build.Repo || eval.Branch != build.Branch || eval.BaseRef != build.BaseRef {
		t.Fatalf("eval spec %+v", eval)
	}
}

// A build and an evaluation of the same goal/seq on one node: the build
// prepares the item's checkout on its write token, the evaluation then
// prepares its own on the read token the platform issues it, and the
// build's credential helper still answers with the write token — so the
// build's push after it goes out on the token it was claimed with (goal
// a-build-pushes-before-it-finishes; the dl360 refusals, ADR-027).
func TestEvaluationLeavesTheBuildsCredentialHelper(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	global := filepath.Join(root, "gitconfig")
	os.WriteFile(global, nil, 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, kv := range []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_TERMINAL_PROMPT=0"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	must := func(dir string, stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	srvRoot := filepath.Join(root, "srv")
	bare := filepath.Join(srvRoot, "acme/widgets.git")
	must(root, "", "init", "--quiet", "--bare", "-b", "main", bare)
	seedDir := filepath.Join(root, "seed")
	must(root, "", "init", "--quiet", "-b", "main", seedDir)
	os.WriteFile(filepath.Join(seedDir, "a"), []byte("a"), 0o644)
	must(seedDir, "", "add", "a")
	must(seedDir, "", "commit", "--quiet", "-m", "a")
	must(seedDir, "", "push", "--quiet", bare, "HEAD:main")
	srv := gatewaytest.GitHub(t, srvRoot, map[string]string{"ghs_build_write": "acme/widgets", "ghs_eval_read": "acme/widgets"})

	// the platform: the build's token issued at claim, the evaluator's
	// once the item is evaluating, both to this node
	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	nodeID := gateway.NodeID(mustUUID(orgID), "test-node").String()
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	issue := func(status, token string) *gateway.GitHubToken {
		t.Helper()
		f.WorkItems[item1]["status"], f.WorkItems[item1]["nodeId"] = status, nodeID
		f.GitHubTokens[item1] = map[string]any{"token": token, "expiresAt": exp, "repo": "acme/widgets"}
		tok, err := gw.IssueItemRepoToken(context.Background(), item1, nodeID)
		if err != nil {
			t.Fatalf("issue %s token: %v", status, err)
		}
		if tok.Token != token {
			t.Fatalf("issued %q, want %q", tok.Token, token)
		}
		return tok
	}

	ctx := context.Background()
	work := filepath.Join(root, "work")
	g := &Git{GitHubURL: srv.URL}
	branch := BranchName("ship-it", 1)

	build := WorkspaceSpec{Root: work, GoalSlug: "ship-it", Seq: 1, Repo: "acme/widgets", Branch: branch, BaseRef: "main"}
	if err := g.Authorize(ctx, build, issue("running", "ghs_build_write")); err != nil {
		t.Fatal(err)
	}
	buildDir, err := g.Prepare(ctx, build)
	if err != nil {
		t.Fatalf("build prepare: %v", err)
	}
	os.WriteFile(filepath.Join(buildDir, "b"), []byte("b"), 0o644)
	must(buildDir, "", "add", "b")
	must(buildDir, "", "commit", "--quiet", "-m", "b")

	// the evaluation prepares before the build has pushed: the dl360 order
	eval := EvalWorkspaceSpec(work, "ship-it", 1, "acme/widgets", branch, "main")
	if err := g.Authorize(ctx, eval, issue("evaluating", "ghs_eval_read")); err != nil {
		t.Fatal(err)
	}
	evalDir, err := g.Prepare(ctx, eval)
	if err != nil {
		t.Fatalf("evaluation prepare: %v", err)
	}

	fill := func(dir string) string {
		t.Helper()
		out := must(dir, "url="+srv.URL+"/acme/widgets.git\n\n", "credential", "fill")
		for _, line := range strings.Split(out, "\n") {
			if p, ok := strings.CutPrefix(line, "password="); ok {
				return p
			}
		}
		t.Fatalf("credential fill in %s answered no password:\n%s", dir, out)
		return ""
	}
	if got := fill(buildDir); got != "ghs_build_write" {
		t.Fatalf("the build's helper answers %q after the evaluation prepared, want ghs_build_write", got)
	}
	if evalDir == buildDir {
		t.Fatalf("the evaluation prepared the build's checkout %s", buildDir)
	}
	if got := fill(evalDir); got != "ghs_eval_read" {
		t.Fatalf("the evaluation's helper answers %q, want ghs_eval_read", got)
	}

	// and the build's push goes out on it
	sha, err := g.Push(ctx, buildDir, branch)
	if err != nil {
		t.Fatalf("the build's push after the evaluation prepared: %v", err)
	}
	if got := must(root, "", "--git-dir", bare, "rev-parse", branch); got != sha {
		t.Fatalf("pushed %s, origin has %s", sha, got)
	}
}
