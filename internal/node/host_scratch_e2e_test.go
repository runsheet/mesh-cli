//go:build !windows

package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// Goal a-host-sessions-scratch-dies-with-the-session, item 6
// (docs/adr/adr-036-a-host-sessions-scratch-dies-with-the-session.md):
// an unsandboxed node's sessions and validations leave nothing in the
// process's /tmp, their scratch under <work root>/.scratch goes when they
// end, the Go build cache they share is measured for `mesh node status`,
// and a node killed mid-session has its dead sessions' scratch swept at
// the next start while an adopted live session's is kept.

// hostScratchYML is one always suite that does what dl360's validations
// did to /tmp: a mktemp, and a `go test` that builds into GOCACHE and
// GOTMPDIR.
const hostScratchYML = `suites:
  - name: unit
    kind: unit
    command: d=$(mktemp -d) && echo x > "$d/left" && go test ./...
    always: true
`

// hostScratchRepo is a Go module with one tiny test, beside the suite.
var hostScratchRepo = map[string]string{
	".runsheet/validate.yml": hostScratchYML,
	"go.mod":                 "module example.com/tiny\n\ngo 1.21\n",
	"tiny.go":                "package tiny\n\nfunc Two() int { return 2 }\n",
	"tiny_test.go":           "package tiny\n\nimport \"testing\"\n\nfunc TestTwo(t *testing.T) {\n\tif Two() != 2 {\n\t\tt.Fatal(Two())\n\t}\n}\n",
}

// tmpCommitHarness is a build session that writes into its TMPDIR as an
// agent's `go test` does, then commits its turn.
type tmpCommitHarness struct{ *commitHarness }

func (h tmpCommitHarness) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	agentWritesTmp(h.t, job.Env)
	return h.commitHarness.Run(ctx, job, on)
}

func TestHostScratchE2E(t *testing.T) {
	t.Run("build, validation and evaluation leave nothing", hostScratchLeavesNothing)
	t.Run("a killed node's dead scratch is swept, a live one kept", hostScratchSweptAtRestart)
}

// An item through build, host validation and evaluation against the fake
// gateway: the process TMPDIR gains no entry, the scratch root is empty
// after, the shared build cache exists with what `go test` put there, and
// the cache trim leaves the reading `mesh node status` prints.
func hostScratchLeavesNothing(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOTOOLCHAIN", "local")
	_, tmpNow := processTmp(t)

	r, _, h, f, item, logs := validatingRunner(t, hostScratchRepo, map[string]string{"tiny.go": "package tiny\n\nfunc Two() int { return 1 + 1 }\n"})
	r.Harness = tmpCommitHarness{h}
	before := tmpNow()

	// the build and its host validation
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatalf("RunItem: %v\n%s", err, logs.String())
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 1 {
		t.Fatalf("finishWorkItem calls = %d, want 1\n%s", n, logs.String())
	}
	a := uploadedAttestation(t, f)
	commands, _ := a["commands"].([]any)
	if len(commands) != 1 || commands[0].(map[string]any)["exitCode"] != float64(0) {
		t.Fatalf("attested commands = %v, want the unit suite passing\n%s", commands, logs.String())
	}
	cache, err := BuildCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	jobs := h.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("build sessions = %d, want 1", len(jobs))
	}
	buildScratch := checkSessionScratch(t, jobs[0], r.WorkRoot, cache)
	checkScratchGone(t, r.WorkRoot, buildScratch, before, tmpNow)

	// the evaluation
	evItem, err := r.GW.WorkItem(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if evItem.Status != "evaluating" {
		t.Fatalf("item is %s after its finish, want evaluating", evItem.Status)
	}
	var evJob harness.Job
	evh := harnessFunc(func(ctx context.Context, j harness.Job, on func(harness.Progress)) (harness.Result, error) {
		evJob = j
		agentWritesTmp(t, j.Env)
		return harness.Result{Success: true, Text: "Holds.\nVERDICT: PASS"}, nil
	})
	ev := &Evaluator{GW: r.GW, Harness: evh, WS: r.WS, Scopes: r.Scopes, WorkRoot: r.WorkRoot, Model: "sonnet",
		Thresholds: func() LimitsConfig { return LimitsConfig{} }, Credentials: r.Credentials, Log: r.Log}
	if err := ev.Evaluate(context.Background(), *evItem); err != nil {
		t.Fatalf("Evaluate: %v\n%s", err, logs.String())
	}
	if evals := f.CallsNamed("evaluateWorkItem"); len(evals) != 1 || evals[0].Input["passed"] != true {
		t.Fatalf("evaluations = %+v, want one passing\n%s", evals, logs.String())
	}
	evScratch := checkSessionScratch(t, evJob, r.WorkRoot, cache)
	checkScratchGone(t, r.WorkRoot, evScratch, before, tmpNow)

	// the shared cache holds what `go test` built, and its size is read
	// for `mesh node status`
	if ents, err := os.ReadDir(cache); err != nil || len(ents) == 0 {
		t.Fatalf("the shared build cache %s is empty or missing: %v", cache, err)
	}
	cacheRoot, _ := SharedCacheRoot()
	scratchRoot, _ := ScratchRoot(r.WorkRoot)
	state := RunState{Dir: t.TempDir()}
	trim := &CacheTrim{Cache: &SharedCache{Root: cacheRoot, MaxBytes: DefaultValidationCacheMaxBytes}, ScratchRoot: scratchRoot, State: &state, Logf: r.Log.Printf}
	if !trim.RunIfDue(context.Background(), time.Now()) {
		t.Fatal("the first cache trim did not run")
	}
	reading, ok := state.CacheSize()
	if !ok || reading.Root != cacheRoot || reading.Bytes <= 0 || reading.Cap != DefaultValidationCacheMaxBytes {
		t.Fatalf("node.cachesize = %+v (%v), want the cache root %s measured against its cap", reading, ok, cacheRoot)
	}
	if reading.ScratchRoot != scratchRoot || reading.ScratchBytes != 0 {
		t.Fatalf("node.cachesize's scratch = %s %d, want %s empty", reading.ScratchRoot, reading.ScratchBytes, scratchRoot)
	}
	if now := tmpNow(); !slices.Equal(now, before) {
		t.Fatalf("the process TMPDIR holds %v at the end, %v before", now, before)
	}
}

// A node killed mid-session, restarted: the start-up sweep removes what a
// session that died with it left — its scratch under the scratch root,
// its mesh-gh-* and mesh-claude-* dirs in /tmp — and keeps the scratch of
// the detached session still running, which the new node adopts; that
// scratch goes once the adopted session's end is handled.
func hostScratchSweptAtRestart(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	t.Setenv("SLOW_EVENTS", "20")
	t.Setenv("SLOW_SLEEP", "0.1")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := seed(t)
	cfg := registeredNode(t, f)
	root := t.TempDir()
	tmp, tmpNow := processTmp(t)
	claude := &countedClaude{ClaudeCode: &harness.ClaudeCode{Binary: tmpWritingClaude(t), Unit: sessionunit.Detached{}}}
	before := tmpNow()

	// node 1 starts the build's detached session, and a second session,
	// in process, has its scratch and config dirs made
	loop1, _ := adoptNode(t, f, cfg, root, claude)
	ctx1, kill1 := context.WithCancel(context.Background())
	ran1 := make(chan error, 1)
	go func() { ran1 <- loop1.Run(ctx1) }()
	var found []FoundSession
	waitFor(t, func() bool {
		found, _ = FindSessions(root, []string{cfg.NodeID})
		return len(found) == 1 && found[0].State.Offset > 0
	})
	scratchRoot, _ := ScratchRoot(root)
	dead := harness.Job{Kind: harness.KindEvaluate}
	deadScratch, err := sessionScratch(nil, scratchRoot, "", harness.KindEvaluate, item2, &dead)
	if err != nil {
		t.Fatal(err)
	}
	agentWritesTmp(t, dead.Env)
	var deadTmp []string
	for _, prefix := range []string{"mesh-gh-", "mesh-claude-"} {
		d, err := os.MkdirTemp(tmp, prefix)
		if err != nil {
			t.Fatal(err)
		}
		deadTmp = append(deadTmp, d)
	}

	// the node is killed: its loop ends, the in-process session's end
	// never runs, the detached one runs on
	kill1()
	select {
	case <-ran1:
	case <-time.After(5 * time.Second):
		t.Fatal("node 1's Run did not return")
	}
	pid := found[0].State.PID
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("the detached session (pid %d) died with its node", pid)
	}
	var liveScratch string
	for _, d := range found[0].State.TempDirs {
		if filepath.Dir(d) == scratchRoot {
			liveScratch = d
		}
	}
	if liveScratch == "" {
		t.Fatalf("state.json's tempDirs %v carry no scratch under %s", found[0].State.TempDirs, scratchRoot)
	}

	// node 2 starts: it finds the live session, sweeps, then adopts
	found, err = FindSessions(root, []string{cfg.NodeID})
	if err != nil || len(found) != 1 || !found[0].Alive() {
		t.Fatalf("found %+v, %v", found, err)
	}
	var lines []string
	res := SweepScratch(ScratchSweep{Roots: []string{scratchRoot}, TempDir: tmp, Found: found, InUse: WorkspaceLive},
		func(format string, args ...any) {
			lines = append(lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
		})
	if res.Dirs != 3 {
		t.Fatalf("swept %+v (%v), want the dead session's scratch and its two /tmp dirs", res, lines)
	}
	if want := fmt.Sprintf("scratch: swept 3 dirs, %d bytes at start (%d kept)", res.Bytes, res.Kept); len(lines) != 1 || lines[0] != want || res.Kept == 0 {
		t.Fatalf("node.log = %q, want %q with the live session's dirs kept", lines, want)
	}
	for _, d := range append([]string{deadScratch}, deadTmp...) {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("the dead session's %s outlived the sweep: %v", d, err)
		}
	}
	if _, err := os.Stat(filepath.Join(liveScratch, "mesh-eval.abc")); err != nil {
		t.Fatalf("the live session's scratch was swept: %v", err)
	}
	for _, d := range found[0].State.TempDirs {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("the live session's temp dir %s was swept: %v", d, err)
		}
	}

	loop2, _ := adoptNode(t, f, cfg, root, claude)
	ctx2, stop2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop2()
	if n := loop2.AdoptSessions(ctx2, found); n != 1 {
		t.Fatalf("adopted %d", n)
	}
	waitFor(t, func() bool { return f.Item(item1)["status"] == "evaluating" })
	loop2.wg.Wait()
	checkScratchGone(t, root, liveScratch, before, tmpNow)
}
