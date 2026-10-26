package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
)

const rebuildValidateYML = `suites:
  - name: item
    kind: unit
    command: "true"
`

// rebuildCase is one item's worktree, built on its branch with a commit,
// whose .git/worktrees/<n> has then been deleted from under it.
type rebuildCase struct {
	f      *gatewaytest.Fake
	r      *Runner
	g      *Git
	item   gateway.WorkItem
	spec   WorkspaceSpec
	cred   *ItemCred
	tok    *gateway.GitHubToken
	dir    string
	admin  string
	head   string
	logged func() string
}

func missingAdminWorkspace(t *testing.T) *rebuildCase {
	t.Helper()
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
	t.Setenv("PATH", filepath.Dir(mustLookPath(t, "git"))+string(os.PathListSeparator)+"/usr/bin:/bin")
	srvRoot := filepath.Join(root, "srv")
	bare := filepath.Join(srvRoot, "acme/widgets.git")
	seedDir := filepath.Join(root, "seed")
	gitOut(t, root, "init", "--quiet", "--bare", "-b", "main", bare)
	gitOut(t, root, "init", "--quiet", "-b", "main", seedDir)
	if err := os.MkdirAll(filepath.Join(seedDir, ".runsheet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, ".runsheet", "validate.yml"), []byte(rebuildValidateYML), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, seedDir, "add", ".")
	gitOut(t, seedDir, "commit", "--quiet", "-m", "main")
	gitOut(t, seedDir, "push", "--quiet", bare, "HEAD:main")
	srv := gatewaytest.GitHub(t, srvRoot, map[string]string{"ghs_claim": "acme/widgets"})

	f := seed(t)
	r, _, item, _ := runnerOnFake(t, f, nil, nil)
	f.GitHubTokens[item1] = map[string]any{"token": "ghs_claim", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "repo": "acme/widgets"}
	var mu sync.Mutex
	var lines []string
	g := &Git{GitHubURL: srv.URL, ReposRoot: filepath.Join(root, "repos"), Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}}
	r.WS = g
	ctx := context.Background()
	spec := WorkspaceSpec{Root: filepath.Join(root, "work"), GoalSlug: "g", Seq: 3, Repo: "acme/widgets", Branch: BranchName("g", 3), BaseRef: "main"}
	// a worktree made before checkouts were clones of their own
	if _, err := prepareWorktree(withGitToken(ctx, srv.URL, &gateway.GitHubToken{Token: "ghs_claim"}), g, spec); err != nil {
		t.Fatalf("worktree: %v", err)
	}
	cred, err := NewItemCred(ctx, r.GW, g, r.Scopes, item, spec, nil, r.logf)
	if err != nil {
		t.Fatalf("item cred: %v", err)
	}
	dir, err := g.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !isWorktree(dir) {
		t.Fatalf("%s is not a worktree", dir)
	}
	// the build's commit on its branch, unpushed, and work not yet committed
	if err := os.WriteFile(filepath.Join(dir, "item.txt"), []byte("item\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "item.txt")
	gitOut(t, dir, "commit", "--quiet", "-m", "the build")
	if err := os.WriteFile(filepath.Join(dir, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	head := gitOut(t, dir, "rev-parse", "HEAD")
	admin := worktreeAdmin(dir)
	if admin == "" {
		t.Fatalf("%s/.git names no admin dir", dir)
	}
	// the admin dir goes from under the live workspace
	if err := os.RemoveAll(admin); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "status").CombinedOutput(); err == nil || !strings.Contains(string(out), "not a git repository") {
		t.Fatalf("git status with the admin dir gone: %v: %s", err, out)
	}
	return &rebuildCase{f: f, r: r, g: g, item: item, spec: spec, cred: cred, tok: cred.Token(), dir: dir, admin: admin, head: head,
		logged: func() string { mu.Lock(); defer mu.Unlock(); return strings.Join(lines, "\n") }}
}

// madeAgain checks the checkout is back as a clone of its own on its
// branch — never a worktree of the shared clone, which gains no
// registration — the removal logged, and nothing failed or released
// against the item.
func (c *rebuildCase) madeAgain(t *testing.T) {
	t.Helper()
	if fi, err := os.Stat(filepath.Join(c.dir, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("%s/.git is not a clone's own git dir: %v", c.dir, err)
	}
	if got := gitOut(t, c.dir, "symbolic-ref", "--short", "HEAD"); got != c.spec.Branch {
		t.Fatalf("HEAD is on %q, want %s", got, c.spec.Branch)
	}
	clone := c.g.seedClone(c.spec)
	if list := gitOut(t, clone, "worktree", "list"); strings.Contains(list, c.dir) {
		t.Fatalf("the shared clone registers %s again:\n%s", c.dir, list)
	}
	line := fmt.Sprintf("workspace %s: worktree %s is gone; made again", c.dir, c.admin)
	if !strings.Contains(c.logged(), line) {
		t.Fatalf("log lacks %q:\n%s", line, c.logged())
	}
	for _, m := range []string{"failWorkItem", "releaseWorkItem"} {
		if calls := c.f.CallsNamed(m); len(calls) != 0 {
			t.Fatalf("the gateway recorded %s for the item: %v", m, calls)
		}
	}
}

// A workspace left by an older node as a worktree whose .git file names a
// deleted .git/worktrees/<n> is never rebuilt as a worktree: it is
// removed and made again as a clone of its own (ADR-028's amendment
// 2026-10-26), with no failure or release recorded against the item.
func TestWorkspaceWithMissingWorktreeAdminIsClonedAgain(t *testing.T) {
	ctx := context.Background()

	t.Run("authorize", func(t *testing.T) {
		c := missingAdminWorkspace(t)
		if err := c.g.Authorize(ctx, c.spec, c.tok); err != nil {
			t.Fatalf("authorize: %v\n%s", err, c.logged())
		}
		if got := gitOut(t, c.dir, "remote", "get-url", "origin"); got != c.g.githubURL()+"/acme/widgets.git" {
			t.Fatalf("origin = %q after the authorize", got)
		}
		if _, err := c.g.Prepare(withGitToken(ctx, c.g.githubURL(), c.tok), c.spec); err != nil {
			t.Fatalf("prepare: %v\n%s", err, c.logged())
		}
		c.madeAgain(t)
	})

	t.Run("prepare (PrepareWithRetry)", func(t *testing.T) {
		c := missingAdminWorkspace(t)
		dir, err := PrepareWithRetry(ctx, c.g, c.spec, c.cred, c.r.logf)
		if err != nil || dir != c.dir {
			t.Fatalf("prepare: %q, %v\n%s", dir, err, c.logged())
		}
		c.madeAgain(t)
	})
}
