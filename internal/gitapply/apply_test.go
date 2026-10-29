package gitapply

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// repo is a temporary repository whose commits are made by run.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	r := &repo{t: t, dir: t.TempDir()}
	r.run("init", "-q", "-b", "main")
	r.write("f.txt", "one\ntwo\nthree\nfour\nfive\nsix\nseven\n")
	r.write("g.txt", "g\n")
	r.commit("base")
	return r
}

func (r *repo) run(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) write(path, body string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, path), []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) commit(msg string) string {
	r.t.Helper()
	r.run("add", "-A")
	r.run("commit", "-q", "--no-gpg-sign", "-m", msg)
	return r.run("rev-parse", "HEAD")
}

func (r *repo) show(rev, path string) string {
	r.t.Helper()
	return r.run("show", rev+":"+path)
}

var ident = Identity{Name: "Mesh", Email: "mesh@example.com", When: time.Date(2026, 10, 29, 1, 2, 3, 0, time.UTC)}

func TestApply(t *testing.T) {
	ctx := context.Background()
	// main moves on f.txt's first line; the change edits its last line
	// and adds a file: the same file, edits that do not overlap
	sameFile := func(t *testing.T) (*repo, string, string) {
		r := newRepo(t)
		r.run("checkout", "-q", "-b", "item")
		r.write("f.txt", "one\ntwo\nthree\nfour\nfive\nsix\nSEVEN\n")
		r.write("new.txt", "new\n")
		head := r.commit("the item's change")
		r.run("checkout", "-q", "main")
		r.write("f.txt", "ONE\ntwo\nthree\nfour\nfive\nsix\nseven\n")
		main := r.commit("main moves")
		return r, main, head
	}

	t.Run("clean apply, linear", func(t *testing.T) {
		r := newRepo(t)
		r.run("checkout", "-q", "-b", "item")
		r.write("new.txt", "new\n")
		head := r.commit("add new")
		r.run("checkout", "-q", "main")
		r.write("g.txt", "g moved\n")
		main := r.commit("main moves")

		steps, conflict, err := Apply(ctx, r.dir, "main", []Change{{Head: head}}, Linear, ident)
		if err != nil || conflict != nil {
			t.Fatalf("Apply: %v %v", err, conflict)
		}
		if len(steps) != 1 {
			t.Fatalf("steps %d, want 1", len(steps))
		}
		s := steps[0]
		if got := r.run("rev-parse", s.Commit+"^@"); got != main {
			t.Errorf("parents %q, want only main %s", got, main)
		}
		if got := r.run("rev-parse", s.Commit+"^{tree}"); got != s.Tree {
			t.Errorf("tree %s, step says %s", got, s.Tree)
		}
		if r.show(s.Commit, "g.txt") != "g moved" || r.show(s.Commit, "new.txt") != "new" {
			t.Errorf("result lacks main's or the change's edit")
		}
		if !reflect.DeepEqual(s.Paths, []string{"new.txt"}) {
			t.Errorf("paths %v, want [new.txt]", s.Paths)
		}
		if got := r.run("log", "-1", "--format=%s|%an|%ae|%cn|%ct", s.Commit); got != "add new|Mesh|mesh@example.com|Mesh|"+strconv.FormatInt(ident.When.Unix(), 10) {
			t.Errorf("commit %q", got)
		}
		if r.run("rev-parse", "main") != main || r.run("rev-parse", "item") != head {
			t.Errorf("Apply moved a ref")
		}
	})

	t.Run("same file, edits that do not overlap", func(t *testing.T) {
		r, _, head := sameFile(t)
		steps, conflict, err := Apply(ctx, r.dir, "main", []Change{{Head: head, Message: "rebased"}}, Linear, ident)
		if err != nil || conflict != nil {
			t.Fatalf("Apply: %v %v", err, conflict)
		}
		if got := r.show(steps[0].Commit, "f.txt"); got != "ONE\ntwo\nthree\nfour\nfive\nsix\nSEVEN" {
			t.Errorf("f.txt %q", got)
		}
		if got := r.run("log", "-1", "--format=%B", steps[0].Commit); got != "rebased" {
			t.Errorf("message %q", got)
		}
		if !reflect.DeepEqual(steps[0].Paths, []string{"f.txt", "new.txt"}) {
			t.Errorf("paths %v", steps[0].Paths)
		}
	})

	t.Run("merge mode parents the tip and the head", func(t *testing.T) {
		r, main, head := sameFile(t)
		steps, conflict, err := Apply(ctx, r.dir, main, []Change{{Head: head}}, Merge, ident)
		if err != nil || conflict != nil {
			t.Fatalf("Apply: %v %v", err, conflict)
		}
		if got := r.run("rev-parse", steps[0].Commit+"^@"); got != main+"\n"+head {
			t.Errorf("parents %q, want [main, head]", got)
		}
		if got := r.run("log", "-1", "--format=%s", steps[0].Commit); got != "Merge "+head {
			t.Errorf("message %q", got)
		}
	})

	t.Run("a real conflict names its paths", func(t *testing.T) {
		r := newRepo(t)
		r.run("checkout", "-q", "-b", "item")
		r.write("f.txt", "uno\ntwo\nthree\nfour\nfive\nsix\nseven\n")
		r.write("g.txt", "g item\n")
		r.write("clean.txt", "clean\n")
		head := r.commit("item")
		r.run("checkout", "-q", "main")
		r.write("f.txt", "ONE\ntwo\nthree\nfour\nfive\nsix\nseven\n")
		r.write("g.txt", "g main\n")
		r.commit("main")
		steps, conflict, err := Apply(ctx, r.dir, "main", []Change{{Head: head}}, Linear, ident)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 0 {
			t.Errorf("steps %v, want none", steps)
		}
		if conflict == nil {
			t.Fatal("no conflict")
		}
		if conflict.Index != 0 || conflict.Head != head || !reflect.DeepEqual(conflict.Paths, []string{"f.txt", "g.txt"}) {
			t.Errorf("conflict %+v", conflict)
		}
	})

	t.Run("a stack of three", func(t *testing.T) {
		r := newRepo(t)
		r.run("checkout", "-q", "-b", "a")
		r.write("a.txt", "a\n")
		a := r.commit("a")
		r.write("b.txt", "b\n")
		b := r.commit("b")
		r.write("f.txt", "one\ntwo\nthree\nfour\nfive\nsix\nseven\nc\n")
		c := r.commit("c")
		r.run("checkout", "-q", "main")
		r.write("f.txt", "ONE\ntwo\nthree\nfour\nfive\nsix\nseven\n")
		main := r.commit("main")

		for _, mode := range []Mode{Linear, Merge} {
			changes := []Change{{Head: a}, {Head: b, Base: a}, {Head: c, Base: b}}
			steps, conflict, err := Apply(ctx, r.dir, "main", changes, mode, ident)
			if err != nil || conflict != nil {
				t.Fatalf("%s: %v %v", mode, err, conflict)
			}
			if len(steps) != 3 {
				t.Fatalf("%s: %d steps", mode, len(steps))
			}
			want := []string{main, steps[0].Commit, steps[1].Commit}
			heads := []string{a, b, c}
			for i, s := range steps {
				parents := r.run("rev-parse", s.Commit+"^@")
				wantParents := want[i]
				if mode == Merge {
					wantParents += "\n" + heads[i]
				}
				if parents != wantParents {
					t.Errorf("%s step %d parents %q, want %q", mode, i, parents, wantParents)
				}
			}
			for i, p := range [][]string{{"a.txt"}, {"b.txt"}, {"f.txt"}} {
				if !reflect.DeepEqual(steps[i].Paths, p) {
					t.Errorf("%s step %d paths %v, want %v", mode, i, steps[i].Paths, p)
				}
			}
			if got := r.show(steps[2].Commit, "f.txt"); got != "ONE\ntwo\nthree\nfour\nfive\nsix\nseven\nc" {
				t.Errorf("%s: f.txt %q", mode, got)
			}
		}
		// in merge mode a stacked change's base is found without being named
		steps, conflict, err := Apply(ctx, r.dir, "main", []Change{{Head: a}, {Head: b}, {Head: c}}, Merge, ident)
		if err != nil || conflict != nil || len(steps) != 3 {
			t.Fatalf("merge, bases found: %v %v %d", err, conflict, len(steps))
		}
		if got := r.show(steps[2].Commit, "b.txt"); got != "b" {
			t.Errorf("b.txt %q", got)
		}
	})

	t.Run("a conflict part way keeps the steps before it", func(t *testing.T) {
		r := newRepo(t)
		r.run("checkout", "-q", "-b", "a")
		r.write("a.txt", "a\n")
		a := r.commit("a")
		r.write("g.txt", "g item\n")
		b := r.commit("b")
		r.run("checkout", "-q", "main")
		r.write("g.txt", "g main\n")
		r.commit("main")
		steps, conflict, err := Apply(ctx, r.dir, "main", []Change{{Head: a}, {Head: b, Base: a}}, Linear, ident)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 1 || conflict == nil || conflict.Index != 1 || conflict.Head != b || !reflect.DeepEqual(conflict.Paths, []string{"g.txt"}) {
			t.Errorf("steps %v conflict %+v", steps, conflict)
		}
	})

	t.Run("identical shas across two runs", func(t *testing.T) {
		r, _, head := sameFile(t)
		var first []Step
		for run := 0; run < 2; run++ {
			for _, mode := range []Mode{Linear, Merge} {
				steps, conflict, err := Apply(ctx, r.dir, "main", []Change{{Head: head}}, mode, ident)
				if err != nil || conflict != nil {
					t.Fatalf("Apply: %v %v", err, conflict)
				}
				if run == 0 {
					first = append(first, steps[0])
					continue
				}
				if want := first[mode]; steps[0].Commit != want.Commit || steps[0].Tree != want.Tree {
					t.Errorf("%s: run 2 wrote %s, run 1 %s", mode, steps[0].Commit, want.Commit)
				}
			}
		}
		// and in another clone of the same history
		clone := t.TempDir()
		if out, err := exec.Command("git", "clone", "-q", "--no-local", r.dir, clone).CombinedOutput(); err != nil {
			t.Fatalf("clone: %v: %s", err, out)
		}
		steps, _, err := Apply(ctx, clone, "origin/main", []Change{{Head: head}}, Linear, ident)
		if err != nil || len(steps) != 1 || steps[0].Commit != first[Linear].Commit {
			t.Errorf("clone wrote %v (%v), want %s", steps, err, first[Linear].Commit)
		}
	})

	t.Run("an unknown head is an error", func(t *testing.T) {
		r := newRepo(t)
		_, conflict, err := Apply(ctx, r.dir, "main", []Change{{Head: strings.Repeat("a", 40)}}, Linear, ident)
		if err == nil || conflict != nil {
			t.Errorf("err %v conflict %v", err, conflict)
		}
	})
}

func TestApplyNeedsGit240(t *testing.T) {
	r := newRepo(t)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	// the hardened git puts its -c flags first, so version is the last word
	fake := "#!/bin/sh\nfor a; do last=$a; done\nif [ \"$last\" = version ]; then echo 'git version 2.39.5 (Apple Git-154)'; exit 0; fi\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	steps, conflict, err := Apply(context.Background(), r.dir, "main", []Change{{Head: "main"}}, Linear, ident)
	if !errors.Is(err, ErrGitTooOld) || steps != nil || conflict != nil {
		t.Errorf("Apply on git 2.39: %v %v %v, want ErrGitTooOld", steps, conflict, err)
	}
	if !strings.Contains(err.Error(), "2.39.5") {
		t.Errorf("error %q does not name the git found", err)
	}
}

func TestVersionOK(t *testing.T) {
	for v, ok := range map[string]bool{
		"git version 2.40.0":                 true,
		"git version 2.54.0 (Apple Git-157)": true,
		"git version 3.0.0":                  true,
		"git version 2.39.5":                 false,
		"git version 1.99.0":                 false,
		"not git":                            false,
	} {
		err := versionOK(v)
		if (err == nil) != ok || err != nil && !errors.Is(err, ErrGitTooOld) {
			t.Errorf("%q: %v, want ok %v", v, err, ok)
		}
	}
}

// Git.Command makes every git Apply runs: a caller's hardened git sees
// each invocation, and the result is the same commit.
func TestApplyOnCallersGit(t *testing.T) {
	r := newRepo(t)
	r.run("checkout", "-q", "-b", "item")
	r.write("new.txt", "new\n")
	head := r.commit("add new")
	r.run("checkout", "-q", "main")
	var ran []string
	g := Git{Command: func(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
		ran = append(ran, firstWord(args))
		cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		return cmd
	}}
	steps, conflict, err := g.Apply(context.Background(), r.dir, "main", []Change{{Head: head}}, Linear, ident)
	if err != nil || conflict != nil {
		t.Fatalf("Apply: %v %v", err, conflict)
	}
	want, _, _ := Apply(context.Background(), r.dir, "main", []Change{{Head: head}}, Linear, ident)
	if steps[0].Commit != want[0].Commit {
		t.Errorf("caller's git wrote %s, plain git %s", steps[0].Commit, want[0].Commit)
	}
	for _, sub := range []string{"version", "merge-tree", "commit-tree", "diff-tree"} {
		if !strings.Contains(strings.Join(ran, " "), sub) {
			t.Errorf("Command never ran %s: %v", sub, ran)
		}
	}
}
