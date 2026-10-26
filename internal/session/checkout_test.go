package session_test

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/node"
	"projectmesh.io/mesh-cli/internal/session"
)

const fakeToken = "ghs_fakeItemToken0123456789"

// TestWorkspaceLeavesNoCredential prepares an item's workspace by the
// checkout step against a local bare repo behind a stand-in GitHub that
// answers only the item's ghs_ token — a build's clone of its own seeded
// from the shared clone, a full clone (an evaluation's), and a rework
// whose checkout an earlier Authorize left a helper in — and walks every
// file under the workspace and its git dir, and the shared clone's
// config: neither the token nor ghs_ appears, each checkout's git dir is
// its own (no worktree), origin is the
// plain https URL, and the node's push still goes out on the token per
// command.
func TestWorkspaceLeavesNoCredential(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, kv := range []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_TERMINAL_PROMPT=0"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	srvRoot := filepath.Join(root, "srv")
	bare := filepath.Join(srvRoot, "acme/widgets.git")
	seed := filepath.Join(root, "seed")
	for _, c := range [][]string{
		{"init", "--quiet", "--bare", "-b", "main", bare},
		{"init", "--quiet", "-b", "main", seed},
		{"-C", seed, "commit", "--quiet", "--allow-empty", "-m", "a"},
		{"-C", seed, "push", "--quiet", bare, "HEAD:main"},
	} {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	srv := gatewaytest.GitHub(t, srvRoot, map[string]string{fakeToken: "acme/widgets"})
	g := &node.Git{GitHubURL: srv.URL, ReposRoot: filepath.Join(root, "repos")}
	tok := &gateway.GitHubToken{Token: fakeToken, ExpiresAt: time.Now().Add(time.Hour), Repository: "acme/widgets"}
	work := filepath.Join(root, "work")
	origin := srv.URL + "/acme/widgets.git"
	ctx := context.Background()

	build := node.WorkspaceSpec{Root: work, GoalSlug: "g", Seq: 1, Repo: "acme/widgets", Branch: node.BranchName("g", 1), BaseRef: "main"}
	clone := node.EvalWorkspaceSpec(work, "g", 2, "acme/widgets", "main-copy", "main")
	clone.Branch = ""
	rework := node.WorkspaceSpec{Root: work, GoalSlug: "g", Seq: 3, Repo: "acme/widgets", Branch: node.BranchName("g", 3), BaseRef: "main"}
	if err := g.Authorize(ctx, rework, tok); err != nil {
		t.Fatalf("authorize the rework's first attempt: %v", err)
	}
	if held, _ := session.ConfigHolding(rework.Dir(), fakeToken); held == "" {
		t.Fatal("Authorize left no helper to clear; the rework case proves nothing")
	}

	for _, c := range []struct {
		name  string
		spec  node.WorkspaceSpec
		build bool
	}{{"build", build, true}, {"full clone", clone, false}, {"rework", rework, true}} {
		t.Run(c.name, func(t *testing.T) {
			spec := c.spec
			state := &session.State{}
			job := session.Job{Steps: []session.Step{session.Checkout(func(ctx context.Context, tok *gateway.GitHubToken) (string, error) {
				return g.Checkout(ctx, spec, tok)
			}, tok)}}
			recs := session.Run(ctx, job, state)
			if recs[0].Outcome != session.Succeeded {
				t.Fatalf("checkout: %s: %v", recs[0].Outcome, recs[0].Err)
			}
			dir := state.WorkspaceDir
			if dir != spec.Dir() {
				t.Fatalf("workspace %q, want %q", dir, spec.Dir())
			}
			gitDir, common, err := session.GitDirs(dir)
			if err != nil {
				t.Fatal(err)
			}
			if gitDir != common {
				t.Fatalf("git dir %s, common %s: the checkout is a worktree, not a clone of its own", gitDir, common)
			}
			// the workspace, its git dir and the shared clone's config:
			// nothing names the token or ghs_
			roots := []string{dir, gitDir}
			for _, r := range roots {
				walkNoSecret(t, r)
			}
			noSecret(t, filepath.Join(common, "config"))
			if shared := filepath.Join(node.SharedClonePath(g.ReposRoot, "acme/widgets"), ".git", "config"); c.build {
				noSecret(t, shared)
			}

			if got := gitOut(t, dir, "remote", "get-url", "origin"); got != origin {
				t.Fatalf("origin %q, want the plain https URL %q", got, origin)
			}
			if got := gitOutOK(dir, "config", "--get-all", "credential."+srv.URL+".helper"); got != "" {
				t.Fatalf("a credential helper is left: %q", got)
			}
			if gitOut(t, dir, "rev-parse", "HEAD") == "" {
				t.Fatal("nothing checked out")
			}
			if !c.build {
				return
			}
			// the node's push still goes out, on the token per command
			if out, err := exec.Command("git", "-C", dir, "commit", "--quiet", "--allow-empty", "-m", "work").CombinedOutput(); err != nil {
				t.Fatalf("commit: %v: %s", err, out)
			}
			if _, err := g.Push(ctx, dir, spec.Branch); err == nil {
				t.Fatal("a push with no token per command went out: the checkout holds a credential")
			}
			if _, err := g.Push(node.OnItemToken(ctx, g, tok), dir, spec.Branch); err != nil {
				t.Fatalf("push on the item token: %v", err)
			}
			for _, r := range roots {
				walkNoSecret(t, r)
			}
			noSecret(t, filepath.Join(common, "config"))
		})
	}
}

// TestCheckoutFailsOnALeftToken: a prepare that leaves the token in the
// checkout's config is the checkout step's failure.
func TestCheckoutFailsOnALeftToken(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[credential]\n\thelper = !echo password="+fakeToken+"\n"), 0o644)
	tok := &gateway.GitHubToken{Token: fakeToken}
	recs := session.Run(context.Background(), session.Job{Steps: []session.Step{session.Checkout(func(context.Context, *gateway.GitHubToken) (string, error) {
		return dir, nil
	}, tok)}}, &session.State{})
	if recs[0].Outcome != session.Failed || recs[0].Err == nil || strings.Contains(recs[0].Err.Error(), fakeToken) {
		t.Fatalf("checkout = %s, %v; want failed, the token unnamed", recs[0].Outcome, recs[0].Err)
	}
}

func walkNoSecret(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		noSecret(t, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func noSecret(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, fakeToken) || strings.Contains(s, "ghs_") {
		t.Errorf("%s names the token", path)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitOutOK(dir string, args ...string) string {
	out, _ := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out))
}
