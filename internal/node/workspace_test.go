package node

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
