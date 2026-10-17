package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Cleanup of a finished item's workspace leaves every worktree whose item
// is claimed, running, evaluating or rebasing on this node untouched —
// its directory and its .git/worktrees/<n> — and so does a bare `git
// worktree prune` run in the shared clone as a session's pod runs it,
// with those worktrees out of its view: each is locked, the clone's gc
// never prunes a worktree, and RemoveWorkspace refuses a live dir.
func TestWorkspaceCleanupNeverRemovesALiveWorktree(t *testing.T) {
	root, origin := lockTestOrigin(t)
	work, repos := filepath.Join(root, "work"), filepath.Join(root, "repos")
	g, cmds := recordingGit(repos)
	clone := SharedClonePath(repos, origin)
	ctx := context.Background()

	live := []gateway.WorkItem{
		{Seq: 1, Status: "claimed"},
		{Seq: 2, Status: "running"},
		{Seq: 3, Status: "evaluating"},
		{Seq: 4, Status: "review", RebaseState: "claimed"},
	}
	finished := gateway.WorkItem{Seq: 5, Status: "merged"}
	prepare := func(item gateway.WorkItem) string {
		t.Helper()
		spec := WorkspaceSpec{Root: work, GoalSlug: "g", Seq: item.Seq, Repo: origin, Branch: BranchName("g", item.Seq), BaseRef: "main"}
		dir, err := g.Prepare(ctx, spec)
		if err != nil {
			t.Fatalf("prepare seq %d: %v", item.Seq, err)
		}
		if !isWorktree(dir) {
			t.Fatalf("seq %d at %s is not a worktree of the shared clone", item.Seq, dir)
		}
		return dir
	}
	type liveTree struct {
		item       gateway.WorkItem
		dir, admin string
		head       string
	}
	var trees []liveTree
	for _, item := range live {
		if !SessionLive(item) {
			t.Fatalf("item %d (%s) is not live by SessionLive", item.Seq, itemStatus(item))
		}
		dir := prepare(item)
		t.Cleanup(HoldWorkspace(dir))
		admin := worktreeAdmin(dir)
		if admin == "" {
			t.Fatalf("seq %d: no admin dir named by %s/.git", item.Seq, dir)
		}
		trees = append(trees, liveTree{item, dir, admin, gitOut(t, dir, "rev-parse", "HEAD")})
	}
	if SessionLive(finished) {
		t.Fatal("the finished item reads as live")
	}
	done := prepare(finished)
	doneAdmin := worktreeAdmin(done)

	// each worktree is locked as its item's, and gc never prunes one
	for _, tr := range trees {
		b, err := os.ReadFile(filepath.Join(tr.admin, "locked"))
		if want := "mesh: item g/" + strconv.Itoa(tr.item.Seq) + " live"; err != nil || strings.TrimSpace(string(b)) != want {
			t.Fatalf("seq %d's lock reads %q (%v), want %q", tr.item.Seq, b, err, want)
		}
	}
	if got := gitOut(t, clone, "config", "--local", "--get", "gc.worktreePruneExpire"); got != "never" {
		t.Fatalf("gc.worktreePruneExpire = %q, want never", got)
	}

	// a live dir is refused outright
	if err := g.RemoveWorkspace(ctx, trees[0].dir); err == nil || !strings.Contains(err.Error(), "live on this node") {
		t.Fatalf("remove of a live workspace: %v, want refused", err)
	}

	// the finished one goes, and only it
	if err := g.RemoveWorkspace(ctx, done); err != nil {
		t.Fatalf("remove the finished workspace: %v", err)
	}
	if _, err := os.Stat(done); !os.IsNotExist(err) {
		t.Fatalf("the finished workspace %s is still there: %v", done, err)
	}
	if _, err := os.Stat(doneAdmin); !os.IsNotExist(err) {
		t.Fatalf("the finished workspace's admin dir %s is still there: %v", doneAdmin, err)
	}
	for _, c := range cmds() {
		if strings.HasPrefix(c, "git worktree prune") {
			t.Fatalf("cleanup ran a blanket prune: %q", c)
		}
	}

	// a pod's view: the shared clone's git dir, no other worktree — a bare
	// prune there sees every live worktree as missing
	hidden := filepath.Join(root, "hidden")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, tr := range trees {
		if err := os.Rename(tr.dir, filepath.Join(hidden, strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "-C", clone, "worktree", "prune", "--verbose").CombinedOutput(); err != nil {
		t.Fatalf("bare prune: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", clone, "gc", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("gc: %v: %s", err, out)
	}
	for i, tr := range trees {
		if err := os.Rename(filepath.Join(hidden, strconv.Itoa(i)), tr.dir); err != nil {
			t.Fatal(err)
		}
	}

	for _, tr := range trees {
		if fi, err := os.Stat(tr.admin); err != nil || !fi.IsDir() {
			t.Fatalf("seq %d (%s): admin dir %s gone: %v", tr.item.Seq, itemStatus(tr.item), tr.admin, err)
		}
		if fi, err := os.Stat(filepath.Join(tr.dir, ".git")); err != nil || fi.IsDir() {
			t.Fatalf("seq %d (%s): %s/.git: %v", tr.item.Seq, itemStatus(tr.item), tr.dir, err)
		}
		if got := gitOut(t, tr.dir, "rev-parse", "HEAD"); got != tr.head {
			t.Fatalf("seq %d (%s): HEAD %s, want %s", tr.item.Seq, itemStatus(tr.item), got, tr.head)
		}
	}
	list := gitOut(t, clone, "worktree", "list")
	if strings.Contains(list, done) {
		t.Fatalf("the finished worktree is still registered:\n%s", list)
	}
}

// Prepare locks each worktree it makes (lockWorktree, from addWorktree and
// from Prepare's reuse branch) before its directory is ever moved: a bare
// `git worktree prune` run in the shared clone, as a session's pod runs it,
// with one item's directory out of its view, leaves that registration in
// place, and the worktree works again once the directory is back.
func TestPreparedWorktreeSurvivesAPruneWithItsDirOutOfView(t *testing.T) {
	root, origin := lockTestOrigin(t)
	work, repos := filepath.Join(root, "work"), filepath.Join(root, "repos")
	g, _ := recordingGit(repos)
	clone := SharedClonePath(repos, origin)
	ctx := context.Background()

	prepare := func(seq int) string {
		t.Helper()
		spec := WorkspaceSpec{Root: work, GoalSlug: "g", Seq: seq, Repo: origin, Branch: BranchName("g", seq), BaseRef: "main"}
		dir, err := g.Prepare(ctx, spec)
		if err != nil {
			t.Fatalf("prepare seq %d: %v", seq, err)
		}
		if !isWorktree(dir) {
			t.Fatalf("seq %d at %s is not a worktree of the shared clone", seq, dir)
		}
		return dir
	}
	dir1 := prepare(1)
	admin1 := worktreeAdmin(dir1)
	if admin1 == "" {
		t.Fatalf("seq 1: no admin dir named by %s/.git", dir1)
	}
	prepare(2)

	hidden := filepath.Join(root, "hidden")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	aside := filepath.Join(hidden, "1")
	if err := os.Rename(dir1, aside); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", clone, "worktree", "prune", "--verbose").CombinedOutput(); err != nil {
		t.Fatalf("prune: %v: %s", err, out)
	}
	if err := os.Rename(aside, dir1); err != nil {
		t.Fatal(err)
	}

	if fi, err := os.Stat(admin1); err != nil || !fi.IsDir() {
		t.Fatalf("admin dir %s gone: %v", admin1, err)
	}
	if out, err := exec.Command("git", "-C", dir1, "status").CombinedOutput(); err != nil {
		t.Fatalf("git status in %s: %v: %s", dir1, err, out)
	}
}
