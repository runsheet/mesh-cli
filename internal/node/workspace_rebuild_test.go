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
	"projectmesh.io/mesh-cli/internal/validate"
)

const rebuildValidateYML = `suites:
  - name: item
    kind: unit
    command: "test -f item.txt"
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
	if out, err := exec.Command("git", "-C", dir, "status").CombinedOutput(); err == nil || !IsWorktreeAdminGone(fmt.Errorf("%s", out)) {
		t.Fatalf("git status with the admin dir gone: %v: %s", err, out)
	}
	return &rebuildCase{f: f, r: r, g: g, item: item, spec: spec, cred: cred, tok: cred.Token(), dir: dir, admin: admin, head: head,
		logged: func() string { mu.Lock(); defer mu.Unlock(); return strings.Join(lines, "\n") }}
}

// rebuilt checks the worktree is back for its branch at the recorded
// head, its files left as they were, the rebuild logged, and nothing
// failed or released against the item.
func (c *rebuildCase) rebuilt(t *testing.T) {
	t.Helper()
	if _, _, gone := worktreeAdminGone(c.dir); gone {
		t.Fatalf("%s still names a gone admin dir", c.dir)
	}
	if got := gitOut(t, c.dir, "symbolic-ref", "--short", "HEAD"); got != c.spec.Branch {
		t.Fatalf("HEAD is on %q, want %s", got, c.spec.Branch)
	}
	if got := gitOut(t, c.dir, "rev-parse", "HEAD"); got != c.head {
		t.Fatalf("HEAD is %s, want the recorded head %s", got, c.head)
	}
	if st := gitOut(t, c.dir, "status", "--porcelain"); st != "?? wip.txt" {
		t.Fatalf("status after the rebuild = %q, want only the untracked wip.txt", st)
	}
	if b, err := os.ReadFile(filepath.Join(worktreeAdmin(c.dir), "locked")); err != nil || !strings.Contains(string(b), "mesh: item g/3 live") {
		t.Fatalf("the rebuilt worktree's lock: %q (%v)", b, err)
	}
	line := fmt.Sprintf("workspace %s: worktree %s is gone; made again for %s at %s", c.dir, c.admin, c.spec.Branch, c.head)
	if !strings.Contains(c.logged(), line) {
		t.Fatalf("log lacks %q:\n%s", line, c.logged())
	}
	for _, m := range []string{"failWorkItem", "releaseWorkItem"} {
		if calls := c.f.CallsNamed(m); len(calls) != 0 {
			t.Fatalf("the gateway recorded %s for the item: %v", m, calls)
		}
	}
}

// A workspace whose .git file names a deleted .git/worktrees/<n> is
// recreated for its branch at its recorded head, and the step that found
// it succeeds — with no failure or release recorded against the item.
func TestWorkspaceWithMissingWorktreeAdminIsRebuilt(t *testing.T) {
	ctx := context.Background()

	t.Run("remote set-url (Authorize)", func(t *testing.T) {
		c := missingAdminWorkspace(t)
		// the shared clone's origin moved, so the authorize must set it again
		clone := worktreeClone(c.dir)
		gitOut(t, clone, "remote", "set-url", "origin", "https://example.invalid/acme/widgets.git")
		if err := c.g.Authorize(ctx, c.spec, c.tok); err != nil {
			t.Fatalf("authorize: %v\n%s", err, c.logged())
		}
		if got := gitOut(t, c.dir, "remote", "get-url", "origin"); got != c.g.githubURL()+"/acme/widgets.git" {
			t.Fatalf("origin = %q after the authorize", got)
		}
		c.rebuilt(t)
	})

	t.Run("prepare (PrepareWithRetry)", func(t *testing.T) {
		c := missingAdminWorkspace(t)
		dir, err := PrepareWithRetry(ctx, c.g, c.spec, c.cred, c.r.logf)
		if err != nil || dir != c.dir {
			t.Fatalf("prepare: %q, %v\n%s", dir, err, c.logged())
		}
		c.rebuilt(t)
	})

	t.Run("rev-parse", func(t *testing.T) {
		c := missingAdminWorkspace(t)
		tries := 0
		tree, err := rebuildAndRetry(ctx, c.g, c.spec, func(bool) (string, error) {
			tries++
			return c.g.run(ctx, c.dir, "git", "rev-parse", c.head+"^{tree}")
		})
		if err != nil || tries != 2 || tree == "" {
			t.Fatalf("rev-parse: %q, %v after %d tries, want one rebuild and one retry\n%s", tree, err, tries, c.logged())
		}
		c.rebuilt(t)
	})

	t.Run("validate", func(t *testing.T) {
		c := missingAdminWorkspace(t)
		vr := c.r.validator()
		var notes []string
		tg := validationTarget{item: c.item, dir: c.dir, base: "main", defaultBranch: "main", prefix: "item g/3", label: "g/3",
			session: func() string { return "s" }, cred: c.cred, spec: c.spec,
			report: func(_ context.Context, note string) { notes = append(notes, note) }}
		v := &buildValidation{runner: validate.HostRunner{Dir: c.dir}, close: func() {}}
		att, err := vr.run(ctx, tg, v)
		if err != nil {
			t.Fatalf("validate: %v\n%s", err, c.logged())
		}
		if att.Commit != c.head {
			t.Fatalf("attested %s, want the recorded head %s", att.Commit, c.head)
		}
		if step, failed := att.Failed(); failed {
			t.Fatalf("validation step %s failed", step)
		}
		c.rebuilt(t)
	})
}
