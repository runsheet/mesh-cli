//go:build !windows

package node

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// processTmp points the test process's TMPDIR at a fresh dir and returns
// it with a reader of what it holds, so a test can say the session left
// nothing there (anything the test itself makes is listed before).
func processTmp(t *testing.T) (string, func() []string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir, func() []string {
		ents, _ := os.ReadDir(dir)
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		return names
	}
}

// agentWritesTmp is what an agent's own `go test` and mktemp do: files
// in $TMPDIR, a read-only dir among them as a Go module cache is.
func agentWritesTmp(t *testing.T, env []string) {
	t.Helper()
	tmp, _ := envOf(env, "TMPDIR")
	if tmp == "" {
		t.Fatalf("no TMPDIR in the session's env: %v", env)
	}
	ro := filepath.Join(tmp, "go-build123", "mod")
	if err := os.MkdirAll(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "x.go"), []byte("package x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
}

// checkSessionScratch says a host session's env carries its own scratch
// as TMPDIR and GOTMPDIR under the work root's scratch root, GOCACHE the
// node's build cache, and its meta names the scratch among its TempDirs.
func checkSessionScratch(t *testing.T, job harness.Job, workRoot, cache string) string {
	t.Helper()
	root, _ := ScratchRoot(workRoot)
	tmp, _ := envOf(job.Env, "TMPDIR")
	if filepath.Dir(tmp) != root || !strings.HasPrefix(filepath.Base(tmp), string(job.Kind)+"-") {
		t.Fatalf("TMPDIR %q is not a %s scratch under %q", tmp, job.Kind, root)
	}
	if v, _ := envOf(job.Env, "GOTMPDIR"); v != tmp {
		t.Fatalf("GOTMPDIR %q, want %q", v, tmp)
	}
	if v, _ := envOf(job.Env, "GOCACHE"); v != cache {
		t.Fatalf("GOCACHE %q, want the shared cache %q", v, cache)
	}
	if !slices.Contains(job.Meta.TempDirs, tmp) {
		t.Fatalf("TempDirs %v do not carry the scratch %q", job.Meta.TempDirs, tmp)
	}
	return tmp
}

func checkScratchGone(t *testing.T, workRoot, scratch string, before []string, tmpNow func() []string) {
	t.Helper()
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch %s outlived its session: %v", scratch, err)
	}
	root, _ := ScratchRoot(workRoot)
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Fatalf("the scratch root holds %v", ents)
	}
	if now := tmpNow(); !slices.Equal(now, before) {
		t.Fatalf("the process TMPDIR holds %v after the session, %v before", now, before)
	}
}

// A host build session runs with its own scratch as TMPDIR and GOTMPDIR
// and the shared build cache as GOCACHE; what the agent leaves there is
// gone when the session ends.
func TestHostBuildSessionScratchDiesWithTheSession(t *testing.T) {
	_, tmpNow := processTmp(t)
	var job harness.Job
	h := harnessFunc(func(ctx context.Context, j harness.Job, on func(harness.Progress)) (harness.Result, error) {
		job = j
		agentWritesTmp(t, j.Env)
		return harness.Result{Success: true, Text: "done"}, nil
	})
	r, _, item, row := runnerOn(t, h, nil)
	r.BuildCache = filepath.Join(t.TempDir(), "go")
	before := tmpNow()
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if row(item1)["status"] != "evaluating" {
		t.Fatalf("row: %v", row(item1))
	}
	scratch := checkSessionScratch(t, job, r.WorkRoot, r.BuildCache)
	checkScratchGone(t, r.WorkRoot, scratch, before, tmpNow)
}

// A host evaluation session gets its scratch the same way, gone with it.
func TestHostEvaluationSessionScratchDiesWithTheSession(t *testing.T) {
	_, tmpNow := processTmp(t)
	f := seed(t)
	issueClaude(f, item1)
	gw := gateway.New(f.URL(), "rs_test")
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: "22222222-2222-4222-8222-222222222222"}
	f.WorkItems[item1]["status"], f.WorkItems[item1]["nodeId"], f.WorkItems[item1]["branch"] = "evaluating", cfg.NodeID, "mesh/ship-it-1"
	var item gateway.WorkItem
	item.ID, item.OrgID, item.GoalID, item.RepoID, item.Seq, item.Branch = item1, orgID, goalID, repoID, 1, "mesh/ship-it-1"
	ws := &fakeWS{root: t.TempDir()}
	var job harness.Job
	h := harnessFunc(func(ctx context.Context, j harness.Job, on func(harness.Progress)) (harness.Result, error) {
		job = j
		agentWritesTmp(t, j.Env)
		return harness.Result{Success: true, Text: "Fine.\nVERDICT: PASS"}, nil
	})
	cache := filepath.Join(t.TempDir(), "go")
	ev := &Evaluator{Credentials: testCreds(gw), GW: gw, Harness: h, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, BuildCache: cache}
	before := tmpNow()
	if err := ev.Evaluate(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if n := evaluateJobsClosed(f); n != 1 || len(f.CallsNamed("evaluateWorkItem")) != 0 {
		t.Fatalf("evaluate jobs closed = %d, calls: %+v", n, f.Calls)
	}
	scratch := checkSessionScratch(t, job, ws.root, cache)
	checkScratchGone(t, ws.root, scratch, before, tmpNow)
}

// A sandboxed session is unchanged: its pod has its own TMPDIR, and the
// node makes no host scratch for it.
func TestSandboxedSessionGetsNoHostScratch(t *testing.T) {
	root := t.TempDir()
	job := harness.Job{Kind: harness.KindBuild, Env: []string{"GH_TOKEN=x"}}
	dir, err := sessionScratch(&sandbox.Sandbox{}, root, filepath.Join(root, "cache"), harness.KindBuild, item1, &job)
	if err != nil || dir != "" {
		t.Fatalf("sessionScratch = %q, %v", dir, err)
	}
	if _, ok := envOf(job.Env, "TMPDIR"); ok || len(job.Env) != 1 {
		t.Fatalf("a sandboxed job's env %v", job.Env)
	}
	if _, err := os.Stat(filepath.Join(root, ScratchDirName)); !os.IsNotExist(err) {
		t.Fatalf("a scratch root made for a sandboxed session: %v", err)
	}
}

// tmpWritingClaude is the slow fake claude, first writing into $TMPDIR
// as an agent's `go test` would — a read-only dir among it.
func tmpWritingClaude(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n" +
		"mkdir -p \"$TMPDIR/go-build1/mod\" && echo x > \"$TMPDIR/go-build1/mod/x.go\" && chmod 555 \"$TMPDIR/go-build1/mod\"\n" +
		"echo x > \"$TMPDIR/mesh-eval.abc\"\n" +
		"exec " + slowClaudeScript(t) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// A detached host build's scratch outlives the node that started it —
// the session still runs in it — and goes when the node that adopts the
// session has handled its end.
func TestAdoptedBuildScratchDiesWithTheSession(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	t.Setenv("SLOW_EVENTS", "20")
	t.Setenv("SLOW_SLEEP", "0.1")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := seed(t)
	cfg := registeredNode(t, f)
	root := t.TempDir()
	_, tmpNow := processTmp(t)
	claude := &countedClaude{ClaudeCode: &harness.ClaudeCode{Binary: tmpWritingClaude(t), Unit: sessionunit.Detached{}}}
	before := tmpNow()

	loop1, _ := adoptNode(t, f, cfg, root, claude)
	ctx1, stop1 := context.WithCancel(context.Background())
	ran1 := make(chan error, 1)
	go func() { ran1 <- loop1.Run(ctx1) }()
	var found []FoundSession
	waitFor(t, func() bool {
		found, _ = FindSessions(root, []string{cfg.NodeID})
		return len(found) == 1 && found[0].State.Offset > 0
	})
	stop1()
	select {
	case <-ran1:
	case <-time.After(5 * time.Second):
		t.Fatal("loop 1's Run did not return")
	}
	pid := found[0].State.PID
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	scratchRoot, _ := ScratchRoot(root)
	var scratch string
	for _, d := range found[0].State.TempDirs {
		if filepath.Dir(d) == scratchRoot {
			scratch = d
		}
	}
	if scratch == "" {
		t.Fatalf("state.json's tempDirs %v carry no scratch under %s", found[0].State.TempDirs, scratchRoot)
	}
	if _, err := os.Stat(filepath.Join(scratch, "mesh-eval.abc")); err != nil {
		t.Fatalf("the detached session's scratch went with its node: %v", err)
	}

	loop2, _ := adoptNode(t, f, cfg, root, claude)
	ctx2, stop2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop2()
	if n := loop2.AdoptSessions(ctx2, found); n != 1 {
		t.Fatalf("adopted %d", n)
	}
	waitFor(t, func() bool { return f.Item(item1)["status"] == "evaluating" })
	loop2.wg.Wait()
	checkScratchGone(t, root, scratch, before, tmpNow)
}
