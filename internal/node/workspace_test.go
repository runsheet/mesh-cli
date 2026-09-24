package node

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
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

// githubStandIn serves bare repos under root over smart HTTP, as GitHub
// would: a request needs basic auth, and a password reaches only the
// repos access grants it ("*" every one) — an installation token its
// one repository, the operator's own login all of them.
func githubStandIn(t *testing.T, root string, access map[string]string) *httptest.Server {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("no git exec path")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("no git-http-backend")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="github"`)
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		repo, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), ".git/")
		if grant := access[pass]; grant != "*" && grant != repo {
			http.Error(w, "Write access to repository not granted.", http.StatusForbidden)
			return
		}
		h := &cgi.Handler{Path: backend, Root: "/", InheritEnv: []string{"PATH"},
			Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=" + user}}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
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
	srv := githubStandIn(t, srvRoot, map[string]string{"ghs_item": "acme/widgets", "operator-login": "*"})

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
	if err := g.Commit(ctx, dir, []string{"b"}, "b"); err != nil {
		t.Fatal(err)
	}
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
