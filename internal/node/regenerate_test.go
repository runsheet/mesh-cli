package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/validate"
)

// regenGenScript writes gen/out.txt from every schema/*.txt, one
// "<name>: <content>" line each — the generator the tests regenerate
// with, so two branches changing different schema files both change
// adjacent lines of the one generated file.
const regenGenScript = `#!/bin/sh
set -e
mkdir -p gen
: > gen/out.txt
for f in schema/*.txt; do
  printf '%s: %s\n' "$(basename "$f" .txt)" "$(cat "$f")" >> gen/out.txt
done
`

type regenRepo struct {
	t      *testing.T
	origin string
	seed   string
}

func regenRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func regenIdentity(t *testing.T, dir string) {
	regenRun(t, dir, "config", "user.name", "test")
	regenRun(t, dir, "config", "user.email", "test@example.com")
	regenRun(t, dir, "config", "commit.gpgsign", "false")
}

func regenWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func regenGenerate(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("sh", "gen.sh")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gen.sh: %v\n%s", err, out)
	}
}

// newRegenRepo makes a bare origin whose main holds two schema files,
// the generator, its output and a hand-written notes.txt.
func newRegenRepo(t *testing.T) *regenRepo {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	r := &regenRepo{t: t, origin: filepath.Join(root, "origin.git"), seed: filepath.Join(root, "seed")}
	regenRun(t, root, "init", "--quiet", "--bare", "--initial-branch=main", r.origin)
	regenRun(t, root, "clone", "--quiet", r.origin, r.seed)
	regenIdentity(t, r.seed)
	regenWrite(t, r.seed, "gen.sh", regenGenScript)
	regenWrite(t, r.seed, "schema/a.txt", "alpha\n")
	regenWrite(t, r.seed, "schema/b.txt", "beta\n")
	regenWrite(t, r.seed, "notes.txt", "one\ntwo\nthree\n")
	regenGenerate(t, r.seed)
	regenRun(t, r.seed, "add", "-A")
	regenRun(t, r.seed, "commit", "--quiet", "-m", "seed")
	regenRun(t, r.seed, "push", "--quiet", "origin", "HEAD:main")
	return r
}

// commit on ref (main, or a branch made from the seed commit) the
// given files, regenerating gen/out.txt as a builder would.
func (r *regenRepo) commit(ref string, files map[string]string) {
	t := r.t
	regenRun(t, r.seed, "fetch", "--quiet", "origin")
	start := "origin/main"
	if out := regenRun(t, r.seed, "ls-remote", "origin", "refs/heads/"+ref); out != "" {
		start = "origin/" + ref
	} else if ref != "main" {
		start = regenRun(t, r.seed, "rev-list", "--max-parents=0", "origin/main")
	}
	regenRun(t, r.seed, "checkout", "--quiet", "--detach", start)
	for rel, content := range files {
		regenWrite(t, r.seed, rel, content)
	}
	regenGenerate(t, r.seed)
	regenRun(t, r.seed, "add", "-A")
	regenRun(t, r.seed, "commit", "--quiet", "-m", "change on "+ref)
	regenRun(t, r.seed, "push", "--quiet", "origin", "HEAD:refs/heads/"+ref)
}

// worktree is the rebase's checkout: a clone detached at
// origin/<branch>, as Rebaser.Rebase makes it.
func (r *regenRepo) worktree(branch string) string {
	t := r.t
	dir := filepath.Join(t.TempDir(), "rebase")
	regenRun(t, filepath.Dir(dir), "clone", "--quiet", r.origin, dir)
	regenIdentity(t, dir)
	regenRun(t, dir, "checkout", "--quiet", "--detach", "origin/"+branch)
	return dir
}

func regenGen(check ...string) validate.Generated {
	return validate.Generated{
		Paths:    []string{"gen/**"},
		Generate: []string{"sh gen.sh"},
		Check:    check,
		Timeout:  time.Minute,
	}
}

func regenNoRebase(t *testing.T, dir string) {
	t.Helper()
	if regenRebaseInProgress(context.Background(), dir) {
		t.Fatal("a rebase is still in progress")
	}
	if st := regenRun(t, dir, "status", "--porcelain", "--untracked-files=no"); st != "" {
		t.Fatalf("worktree not clean:\n%s", st)
	}
}

func TestRegenerateRebase(t *testing.T) {
	ctx := context.Background()
	const branch = "mesh/feature-1"

	t.Run("generated-only conflict is regenerated", func(t *testing.T) {
		r := newRegenRepo(t)
		r.commit(branch, map[string]string{"schema/b.txt": "branch beta\n"})
		r.commit("main", map[string]string{"schema/a.txt": "main alpha\n"})
		dir := r.worktree(branch)

		out, err := regenerateRebase(ctx, dir, branch, "main", regenGen("sh gen.sh && git diff --exit-code -- gen"), validate.HostRunner{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != RegenRegenerated {
			t.Fatalf("kind = %s (%s), want regenerated", out.Kind, out)
		}
		if strings.Join(out.Paths, ",") != "gen/out.txt" {
			t.Errorf("paths = %v, want [gen/out.txt]", out.Paths)
		}
		if head := regenRun(t, dir, "rev-parse", "HEAD"); out.Head != head {
			t.Errorf("head = %s, HEAD is %s", out.Head, head)
		}
		if got := regenRun(t, dir, "show", "HEAD:gen/out.txt"); got != "a: main alpha\nb: branch beta" {
			t.Errorf("HEAD:gen/out.txt = %q, want the generator's output over main's and the branch's schema", got)
		}
		if mb := regenRun(t, dir, "merge-base", "HEAD", "origin/main"); mb != regenRun(t, dir, "rev-parse", "origin/main") {
			t.Errorf("HEAD is not on top of origin/main")
		}
		regenNoRebase(t, dir)
	})

	t.Run("non-generated conflict is not generated", func(t *testing.T) {
		r := newRegenRepo(t)
		r.commit(branch, map[string]string{"schema/b.txt": "branch beta\n", "notes.txt": "one\nbranch\nthree\n"})
		r.commit("main", map[string]string{"schema/a.txt": "main alpha\n", "notes.txt": "one\nmain\nthree\n"})
		dir := r.worktree(branch)

		out, err := regenerateRebase(ctx, dir, branch, "main", regenGen("true"), validate.HostRunner{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != RegenNotGenerated {
			t.Fatalf("kind = %s (%s), want not_generated", out.Kind, out)
		}
		if strings.Join(out.Paths, ",") != "notes.txt" {
			t.Errorf("paths = %v, want [notes.txt]", out.Paths)
		}
		if head, want := regenRun(t, dir, "rev-parse", "HEAD"), regenRun(t, dir, "rev-parse", "origin/"+branch); head != want {
			t.Errorf("HEAD = %s, want origin/%s %s", head, branch, want)
		}
		regenNoRebase(t, dir)
	})

	t.Run("failing check is check failed", func(t *testing.T) {
		r := newRegenRepo(t)
		r.commit(branch, map[string]string{"schema/b.txt": "branch beta\n"})
		r.commit("main", map[string]string{"schema/a.txt": "main alpha\n"})
		dir := r.worktree(branch)

		const failing = "echo duplicate field Blobs >&2; exit 3"
		out, err := regenerateRebase(ctx, dir, branch, "main", regenGen("true", failing), validate.HostRunner{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != RegenCheckFailed {
			t.Fatalf("kind = %s (%s), want check_failed", out.Kind, out)
		}
		if out.Command != failing {
			t.Errorf("command = %q, want %q", out.Command, failing)
		}
		if !strings.Contains(out.Output, "duplicate field Blobs") || !strings.Contains(out.Output, "exit 3") {
			t.Errorf("output = %q, want the command's exit and output", out.Output)
		}
		if head, want := regenRun(t, dir, "rev-parse", "HEAD"), regenRun(t, dir, "rev-parse", "origin/"+branch); head != want {
			t.Errorf("HEAD = %s, want origin/%s %s", head, branch, want)
		}
		regenNoRebase(t, dir)
	})

	t.Run("no conflict is clean", func(t *testing.T) {
		r := newRegenRepo(t)
		r.commit(branch, map[string]string{"notes.txt": "branch\ntwo\nthree\n"})
		r.commit("main", map[string]string{"extra.txt": "main\n"})
		dir := r.worktree(branch)

		out, err := regenerateRebase(ctx, dir, branch, "main", regenGen("false"), validate.HostRunner{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != RegenClean {
			t.Fatalf("kind = %s (%s), want clean", out.Kind, out)
		}
		if head := regenRun(t, dir, "rev-parse", "HEAD"); out.Head != head {
			t.Errorf("head = %s, HEAD is %s", out.Head, head)
		}
		if regenRun(t, dir, "show", "HEAD:extra.txt") != "main" {
			t.Error("HEAD does not carry main's change")
		}
		regenNoRebase(t, dir)
	})
}
