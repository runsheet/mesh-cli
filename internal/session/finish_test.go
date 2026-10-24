package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// fakeChangesets is the platform's changeset half in memory: what it
// holds, what it was asked, what was put, recorded and finished.
type fakeChangesets struct {
	mu          sync.Mutex
	unsupported bool
	held        map[gateway.ChangesetObject][]byte
	asked       [][]gateway.ChangesetObject
	put         []gateway.ChangesetObject
	recorded    []gateway.Changeset
	finished    []string // changeset ids
	bases       []string // the base commit each finish named, "" for none
}

func (g *fakeChangesets) gone() error {
	return &gateway.Error{Message: "404 page not found", Unsupported: true, Status: 404}
}

func (g *fakeChangesets) ChangesetMissing(_ context.Context, _ string, objects []gateway.ChangesetObject) ([]gateway.ChangesetObject, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unsupported {
		return nil, g.gone()
	}
	g.asked = append(g.asked, objects)
	var missing []gateway.ChangesetObject
	for _, o := range objects {
		if _, ok := g.held[o]; !ok {
			missing = append(missing, o)
		}
	}
	return missing, nil
}

func (g *fakeChangesets) PutChangesetObject(_ context.Context, _ string, o gateway.ChangesetObject, content []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.put = append(g.put, o)
	g.held[o] = content
	return nil
}

func (g *fakeChangesets) RecordChangeset(_ context.Context, _ string, cs gateway.Changeset) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.recorded = append(g.recorded, cs)
	return nil
}

func (g *fakeChangesets) FinishWorkItemChangeset(_ context.Context, _, _, id, baseSHA string, _ int, _ *gateway.UsageReport, _ ...string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.finished = append(g.finished, id)
	g.bases = append(g.bases, baseSHA)
	return nil
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, path, body string) {
	t.Helper()
	p := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// builtWorkspace is a checkout on base with the builder's change: a.txt
// changed and dir/c.txt added in a commit, keep.txt removed and
// other/d.txt added uncommitted.
func builtWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.txt", "one\n")
	write(t, dir, "dir/b.txt", "bee\n")
	write(t, dir, "keep.txt", "keep\n")
	write(t, dir, "other/e.txt", "e\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "base")
	git(t, dir, "tag", "base")
	write(t, dir, "a.txt", "two\n")
	write(t, dir, "dir/c.txt", "sea\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "build")
	if err := os.Remove(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "other/d.txt", "dee\n")
	return dir
}

func TestFinishRecordsAChangeset(t *testing.T) {
	dir := builtWorkspace(t)
	baseTree := git(t, dir, "rev-parse", "base^{tree}")
	// the platform already holds a.txt's new content, as it would a blob
	// some earlier changeset carried
	held := gateway.ChangesetObject{Type: gateway.ObjectBlob, SHA: git(t, dir, "hash-object", "a.txt")}
	gw := &fakeChangesets{held: map[gateway.ChangesetObject][]byte{held: []byte("two\n")}}

	f := &Finish{ItemID: "item-6", Branch: "mesh/g-6", ParentChangesetID: "cs-5", Gateway: gw, NewID: func() string { return "cs-6" }}
	state := &State{WorkspaceDir: dir, BaseRef: "base"}
	records := Run(context.Background(), Job{Steps: []Step{f.Step()}}, state)
	if records[0].Outcome != Succeeded {
		t.Fatalf("finish: %s: %v", records[0].Outcome, records[0].Err)
	}

	// the tree recorded is the workspace's, uncommitted changes included
	git(t, dir, "commit", "-qam", "rest")
	tree := git(t, dir, "rev-parse", "HEAD^{tree}")

	if len(gw.asked) != 1 {
		t.Fatalf("missing asked %d times, want 1", len(gw.asked))
	}
	// offered: the root tree, the changed subtrees and the new blobs —
	// never the unchanged dir/b.txt or other/e.txt
	want := []gateway.ChangesetObject{
		{Type: gateway.ObjectBlob, SHA: git(t, dir, "rev-parse", "HEAD:a.txt")},
		{Type: gateway.ObjectBlob, SHA: git(t, dir, "rev-parse", "HEAD:dir/c.txt")},
		{Type: gateway.ObjectBlob, SHA: git(t, dir, "rev-parse", "HEAD:other/d.txt")},
		{Type: gateway.ObjectTree, SHA: tree},
		{Type: gateway.ObjectTree, SHA: git(t, dir, "rev-parse", "HEAD:dir")},
		{Type: gateway.ObjectTree, SHA: git(t, dir, "rev-parse", "HEAD:other")},
	}
	offered := map[gateway.ChangesetObject]bool{}
	for _, o := range gw.asked[0] {
		offered[o] = true
	}
	if len(offered) != len(want) {
		t.Fatalf("offered %+v, want %+v", gw.asked[0], want)
	}
	for _, o := range want {
		if !offered[o] {
			t.Fatalf("offered %+v, missing %+v", gw.asked[0], o)
		}
	}
	unchanged := gateway.ChangesetObject{Type: gateway.ObjectBlob, SHA: git(t, dir, "rev-parse", "HEAD:dir/b.txt")}
	if offered[unchanged] {
		t.Fatalf("offered the unchanged dir/b.txt")
	}

	// only what the platform lacked was put: every offered object but
	// the held a.txt, each with git's own bytes
	if len(gw.put) != len(want)-1 {
		t.Fatalf("put %+v, want every offered object but %s", gw.put, held.SHA)
	}
	for _, o := range gw.put {
		if o == held {
			t.Fatalf("put the held blob %s", o.SHA)
		}
		got := gw.held[o]
		wantBytes := []byte(git(t, dir, "cat-file", o.Type, o.SHA))
		if o.Type == gateway.ObjectBlob && strings.TrimSpace(string(got)) != string(wantBytes) {
			t.Fatalf("%s uploaded %q, want %q", o.SHA, got, wantBytes)
		}
	}
	rootContent, err := exec.Command("git", "-C", dir, "cat-file", "tree", tree).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(gw.held[gateway.ChangesetObject{Type: gateway.ObjectTree, SHA: tree}]) != string(rootContent) {
		t.Fatal("the root tree uploaded is not git's own bytes of it")
	}

	if len(gw.recorded) != 1 {
		t.Fatalf("recorded %d changesets, want 1", len(gw.recorded))
	}
	cs := gw.recorded[0]
	wantCS := gateway.Changeset{
		ChangesetID:       "cs-6",
		BaseTree:          baseTree,
		Tree:              tree,
		PathsChanged:      []string{"a.txt", "dir/c.txt", "keep.txt", "other/d.txt"},
		ParentChangesetID: "cs-5",
	}
	if !reflect.DeepEqual(cs, wantCS) {
		t.Fatalf("recorded %+v, want %+v", cs, wantCS)
	}
	if !reflect.DeepEqual(gw.finished, []string{"cs-6"}) {
		t.Fatalf("finished on %v, want [cs-6]", gw.finished)
	}
	// the finish names the base commit the platform writes the branch
	// on: base's commit, whose tree is the changeset's base_tree
	baseSHA := git(t, dir, "rev-parse", "base^{commit}")
	if !reflect.DeepEqual(gw.bases, []string{baseSHA}) {
		t.Fatalf("finished naming base %v, want [%s]", gw.bases, baseSHA)
	}
	res := state.Results[FinishStepName].(*FinishResult)
	if res.ChangesetID != "cs-6" || res.Pushed || res.BaseSHA != baseSHA {
		t.Fatalf("result %+v", res)
	}
}

// The base commit the finish names is the one the change was built on,
// and the changeset's base_tree is that commit's tree — even when the
// base ref moved on since the checkout (another item's fetch of the
// shared clone), which is logged, not followed; a rework continued from
// an earlier attempt's branch, forked off an older base, names the fork
// point; and with no checkout-time sha on the state the merge-base is
// read back.
func TestFinishNamesTheBaseTheChangeWasBuiltOn(t *testing.T) {
	// upstream at U1; the branch forks there and commits B1; upstream
	// then moves on to U2
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "upstream")
	write(t, dir, "a.txt", "one\n")
	write(t, dir, "shared.txt", "shared\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "U1")
	u1 := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "checkout", "-qb", "mesh/g-7")
	write(t, dir, "a.txt", "two\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "B1")
	git(t, dir, "checkout", "-q", "upstream")
	write(t, dir, "shared.txt", "moved on\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "U2")
	u2 := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "checkout", "-q", "mesh/g-7")
	u1Tree := git(t, dir, "rev-parse", u1+"^{tree}")

	cases := []struct {
		name     string
		checkout string // State.BaseSHA
		wantBase string
		wantLog  bool
	}{
		{"checkout sha, base moved on", u1, u1, true},
		{"no checkout sha: the merge-base", "", u1, true},
		// a rework's checkout fetched upstream at U2, which the branch
		// does not descend from: the fork point, not U2
		{"checkout sha not an ancestor: the merge-base", u2, u1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logged []string
			gw := &fakeChangesets{held: map[gateway.ChangesetObject][]byte{}}
			f := &Finish{ItemID: "item-7", Branch: "mesh/g-7", Gateway: gw, NewID: func() string { return "cs-7" },
				Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }}
			state := &State{WorkspaceDir: dir, BaseRef: "upstream", BaseSHA: tc.checkout}
			records := Run(context.Background(), Job{Steps: []Step{f.Step()}}, state)
			if records[0].Outcome != Succeeded {
				t.Fatalf("finish: %s: %v", records[0].Outcome, records[0].Err)
			}
			if !reflect.DeepEqual(gw.bases, []string{tc.wantBase}) {
				t.Fatalf("finish named base %v, want [%s]", gw.bases, tc.wantBase)
			}
			cs := gw.recorded[0]
			// the base_tree is the named commit's own, and the paths are
			// the builder's alone: upstream's later change is not among
			// them as a revert
			if cs.BaseTree != u1Tree || !reflect.DeepEqual(cs.PathsChanged, []string{"a.txt"}) {
				t.Fatalf("recorded %+v, want base tree %s and a.txt alone", cs, u1Tree)
			}
			if tc.wantLog != (len(logged) == 1) || (tc.wantLog && !strings.Contains(logged[0], "upstream is at "+u2)) {
				t.Fatalf("logged %q", logged)
			}
		})
	}

	// the base still where the checkout left it: named, nothing logged
	t.Run("base unmoved", func(t *testing.T) {
		git(t, dir, "branch", "-f", "upstream", u1)
		var logged []string
		gw := &fakeChangesets{held: map[gateway.ChangesetObject][]byte{}}
		f := &Finish{ItemID: "item-7", Branch: "mesh/g-7", Gateway: gw, NewID: func() string { return "cs-7" },
			Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }}
		state := &State{WorkspaceDir: dir, BaseRef: "upstream", BaseSHA: u1}
		if records := Run(context.Background(), Job{Steps: []Step{f.Step()}}, state); records[0].Outcome != Succeeded {
			t.Fatalf("finish: %s: %v", records[0].Outcome, records[0].Err)
		}
		if !reflect.DeepEqual(gw.bases, []string{u1}) || gw.recorded[0].BaseTree != u1Tree || len(logged) != 0 {
			t.Fatalf("bases %v, base tree %s, logged %q", gw.bases, gw.recorded[0].BaseTree, logged)
		}
	})
}

// baseCommit's tree is always the commit's it names: the base ref's
// own commit and tree when nothing moved; the checkout-time commit's
// when the ref moved past it; the merge-base's for a branch that does
// not descend from the checkout-time commit; a ref with no commit is
// the step's error.
func TestBaseCommitTreeIsTheCommitsOwn(t *testing.T) {
	dir := builtWorkspace(t)
	ctx := context.Background()
	base := git(t, dir, "rev-parse", "base^{commit}")
	baseTree := git(t, dir, "rev-parse", "base^{tree}")
	head := git(t, dir, "rev-parse", "HEAD")
	headTree := git(t, dir, "rev-parse", "HEAD^{tree}")

	var logged int
	logf := func(string, ...any) { logged++ }
	sha, tree, err := baseCommit(ctx, RunGit, dir, "base", "", logf)
	if err != nil || sha != base || tree != baseTree || logged != 0 {
		t.Fatalf("no checkout sha: %s %s %v, logged %d", sha, tree, err, logged)
	}
	sha, tree, err = baseCommit(ctx, RunGit, dir, "base", base, logf)
	if err != nil || sha != base || tree != baseTree || logged != 0 {
		t.Fatalf("checkout sha = base: %s %s %v, logged %d", sha, tree, err, logged)
	}
	// the ref moved to HEAD itself (as a fetch would move origin/main
	// past the fork point): the checkout-time commit holds, its tree
	// with it, and the move is logged once
	git(t, dir, "tag", "-f", "base", head)
	sha, tree, err = baseCommit(ctx, RunGit, dir, "base", base, logf)
	if err != nil || sha != base || tree != baseTree || logged != 1 {
		t.Fatalf("ref moved: %s %s %v, logged %d", sha, tree, err, logged)
	}
	// a checkout-time commit HEAD does not descend from: the merge-base
	// of HEAD and the ref — here HEAD itself
	git(t, dir, "checkout", "-q", "--orphan", "elsewhere")
	git(t, dir, "commit", "-qm", "unrelated", "--allow-empty")
	stray := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "checkout", "-q", "main")
	sha, tree, err = baseCommit(ctx, RunGit, dir, "base", stray, logf)
	if err != nil || sha != head || tree != headTree || logged != 1 {
		t.Fatalf("checkout sha not an ancestor: %s %s %v, logged %d", sha, tree, err, logged)
	}
	if _, _, err := baseCommit(ctx, RunGit, dir, "no-such-ref", "", logf); err == nil || !strings.Contains(err.Error(), "base commit of no-such-ref") {
		t.Fatalf("missing ref: %v", err)
	}
}

// The builder's session runs in the pod; the finish step never does.
// The changeset path runs no git push anywhere, and the fallback for a
// platform without changesets pushes from the node process, not by exec
// in the job container.
func TestFinishNeverPushesFromTheJobContainer(t *testing.T) {
	for _, unsupported := range []bool{false, true} {
		name := "changeset"
		if unsupported {
			name = "fallback push"
		}
		t.Run(name, func(t *testing.T) {
			dir := builtWorkspace(t)
			rt := &sandbox.Fake{}
			sb := &sandbox.Sandbox{Runtime: rt, Config: sandbox.Config{Image: "ghcr.io/runsheet/mesh-node:v1"}, TempDir: t.TempDir()}
			pod, err := sb.Open("item-6", "build", sandbox.GitIdentity{})
			if err != nil {
				t.Fatal(err)
			}
			defer pod.Close()

			var gitCalls [][]string
			recordGit := func(ctx context.Context, d string, args ...string) ([]byte, error) {
				gitCalls = append(gitCalls, args)
				return RunGit(ctx, d, args...)
			}
			var pushed []string
			gw := &fakeChangesets{held: map[gateway.ChangesetObject][]byte{}, unsupported: unsupported}
			f := &Finish{
				ItemID: "item-6", Branch: "mesh/g-6", Gateway: gw, Git: recordGit,
				Push: func(ctx context.Context, d, branch string) (string, error) {
					pushed = append(pushed, branch)
					return "deadbeef", nil
				},
				FinishPushed: func(context.Context, string, string, string) error { return nil },
			}
			job := Job{Steps: []Step{
				{Name: "run", Run: func(ctx context.Context, s *State) error {
					return s.Pod.(*sandbox.Session).Exec(ctx, sandbox.RunSpec{Workspace: s.WorkspaceDir, Command: []string{"claude", "-p"}})
				}},
				f.Step(),
			}}
			state := &State{WorkspaceDir: dir, BaseRef: "base", Pod: pod}
			records := Run(context.Background(), job, state)
			for _, r := range records {
				if r.Outcome != Succeeded {
					t.Fatalf("step %s: %s: %v", r.Name, r.Outcome, r.Err)
				}
			}

			// the pod ran the builder alone: no git push, no gh
			if len(rt.Agents) != 1 || rt.Agents[0].Command[0] != "claude" {
				t.Fatalf("pod ran %d commands: %+v", len(rt.Agents), rt.Agents)
			}
			for _, a := range rt.Agents {
				cmd := strings.Join(a.Command, " ")
				if strings.Contains(cmd, "git push") || strings.Contains(cmd, "push") || a.Command[0] == "gh" || strings.Contains(cmd, " gh ") {
					t.Fatalf("the job container ran %q", cmd)
				}
			}
			for _, c := range rt.Calls {
				if strings.HasPrefix(c, "exec ") {
					t.Fatalf("finish exec'd in the pod: %s", c)
				}
			}
			for _, args := range gitCalls {
				if len(args) > 0 && args[0] == "push" {
					t.Fatalf("finish ran git %v", args)
				}
			}

			res := state.Results[FinishStepName].(*FinishResult)
			if unsupported {
				if !reflect.DeepEqual(pushed, []string{"mesh/g-6"}) || !res.Pushed || res.HeadSHA != "deadbeef" {
					t.Fatalf("fallback: pushed %v, result %+v", pushed, res)
				}
				return
			}
			if len(pushed) != 0 || res.Pushed || len(gw.recorded) != 1 || len(gw.finished) != 1 {
				t.Fatalf("changeset path pushed %v; recorded %d, finished %d", pushed, len(gw.recorded), len(gw.finished))
			}
		})
	}
}
