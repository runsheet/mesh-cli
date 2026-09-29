//go:build !windows

package node

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// countedClaude is the real Claude Code harness on item 1's fake claude
// script, counting the sessions it starts (Attach is ClaudeCode's own).
type countedClaude struct {
	*harness.ClaudeCode
	starts atomic.Int32
}

func (c *countedClaude) Run(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
	c.starts.Add(1)
	return c.ClaudeCode.Run(ctx, job, on)
}

func slowClaudeScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../harness/testdata/slow_claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// adoptNode is one node process on f: its loop, runner and the adopter
// the node wires, on the shared work root.
func adoptNode(t *testing.T, f *gatewaytest.Fake, cfg *Config, root string, h harness.Harness) (*Loop, *Runner) {
	t.Helper()
	gw := gateway.New(f.URL(), "rs_test")
	logger := log.New(os.Stderr, "", 0)
	runner := &Runner{GW: gw, Harness: h, WS: &fakeWS{root: root}, Cfg: cfg, Scopes: scopesOf(cfg), WorkRoot: root, ProgressEvery: 50 * time.Millisecond, Log: logger}
	loop := &Loop{GW: gw, Cfg: cfg, Scopes: scopesOf(cfg), Version: "test", Harness: "claude-code", Interval: time.Hour, Log: logger,
		Dispatch: func(ctx context.Context, item gateway.WorkItem) { _ = runner.RunItem(ctx, item) }}
	loop.AdoptSession = AdoptWith{GW: gw, Scopes: loop.CurrentScopes, Logf: logger.Printf, Build: runner.AdoptItem}.Adopt
	return loop, runner
}

func registeredNode(t *testing.T, f *gatewaytest.Fake) *Config {
	t.Helper()
	cfg := &Config{OrgLogin: "acme", OrgID: orgID, AccountID: acctID, Slug: "test-node", NodeID: gateway.NodeID(mustUUID(orgID), "test-node").String(), Capacity: Fixed(1)}
	gw := gateway.New(f.URL(), "rs_test")
	if _, err := gw.RegisterNode(context.Background(), gateway.RegisterNodeInput{NodeID: cfg.NodeID, OrgID: orgID, OwnerID: f.UserID, Name: "n", Slug: "test-node", Harnesses: []string{"claude-code"}, Capacity: 1, Version: "test"}); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// (a) A build session started by one loop keeps running when that loop's
// Run is cancelled; a second loop on the same work root adopts it,
// reports its progress, and finishes the item from its output — one
// claim, one claude start, one finish. (c) The item's heartbeats go on
// across the restart. (d) The node's heartbeat lists the adopted session
// while it runs, and has no `adopted` once it has ended.
func TestAdoptedBuildFinishesAcrossARestart(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	t.Setenv("SLOW_EVENTS", "30")
	t.Setenv("SLOW_SLEEP", "0.1")
	f := seed(t)
	cfg := registeredNode(t, f)
	root := t.TempDir()
	claude := &countedClaude{ClaudeCode: &harness.ClaudeCode{Binary: slowClaudeScript(t), Unit: sessionunit.Detached{}}}

	// process 1 claims the item and starts its session, then stops
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
	restartAt := time.Now()
	pid := found[0].State.PID
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("the session (pid %d) died with its node", pid)
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 0 {
		t.Fatalf("finished before the restart: %d", n)
	}

	// process 2 finds it and takes it up
	loop2, _ := adoptNode(t, f, cfg, root, claude)
	found, err := FindSessions(root, []string{cfg.NodeID})
	if err != nil || len(found) != 1 || found[0].State.ItemID != item1 || found[0].State.Kind != "build" || !found[0].Alive() {
		t.Fatalf("found %+v, %v", found, err)
	}
	ctx2, stop2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop2()
	if n := loop2.AdoptSessions(ctx2, found); n != 1 {
		t.Fatalf("adopted %d", n)
	}
	if loop2.Running() != 1 {
		t.Fatalf("Running() = %d, want the adopted build", loop2.Running())
	}
	loop2.mu.Lock()
	inFlight := loop2.sessions()
	loop2.mu.Unlock()
	if inFlight != 1 {
		t.Fatalf("sessions() = %d", inFlight)
	}
	loop2.Tick(ctx2) // the node's heartbeat; capacity 1 is taken, nothing is claimed
	hb := f.CallsNamed("heartbeatNode")
	adopted, _ := hb[len(hb)-1].Input["adopted"].([]any)
	if len(adopted) != 1 {
		t.Fatalf("heartbeat adopted = %#v", hb[len(hb)-1].Input["adopted"])
	}
	if a := adopted[0].(map[string]any); a["itemId"] != item1 || a["kind"] != "build" || a["since"] == "" {
		t.Fatalf("adopted = %#v", a)
	}

	waitFor(t, func() bool { return f.Item(item1)["status"] == "evaluating" })
	loop2.wg.Wait()
	if n := len(f.CallsNamed("finishWorkItem")); n != 1 {
		t.Fatalf("finishWorkItem calls = %d, want 1", n)
	}
	if n := len(f.CallsNamed("claimWorkItem")); n != 1 {
		t.Fatalf("claimWorkItem calls = %d, want 1", n)
	}
	if n := claude.starts.Load(); n != 1 {
		t.Fatalf("claude started %d times", n)
	}
	if row := f.Item(item1); row["branch"] != "mesh/ship-it-1" || row["headSha"] != "abc123def456" || row["costCents"] != 42 {
		t.Fatalf("finished row: %v", row)
	}
	// progress after the restart, with the session's text or its cost
	var after []gatewaytest.Call
	var gap time.Duration
	var last time.Time
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		if c.Input["aggregateId"] != item1 {
			continue
		}
		if !last.IsZero() && c.At.Sub(last) > gap {
			gap = c.At.Sub(last)
		}
		last = c.At
		if c.At.After(restartAt) {
			after = append(after, c)
		}
	}
	said := false
	for _, c := range after {
		if n, _ := c.Input["note"].(string); strings.HasPrefix(n, "step ") || n == "all done" || c.Input["costCents"] != float64(0) {
			said = true
		}
	}
	if !said {
		t.Fatalf("no progress with the session's text or cost after the restart: %+v", after)
	}
	// the stall window of this test: no gap in the item's heartbeats as
	// long, across the restart included
	if stall := 2 * time.Second; gap > stall {
		t.Fatalf("heartbeats stopped for %s (stall window %s)", gap, stall)
	}
	if _, err := os.Stat(sessionunit.Dir(found[0].State.Workspace)); !os.IsNotExist(err) {
		t.Fatalf("session dir left after the finish: %v", err)
	}
	loop2.Tick(ctx2)
	hb = f.CallsNamed("heartbeatNode")
	if v, ok := hb[len(hb)-1].Input["adopted"]; ok {
		t.Fatalf("adopted after the session ended: %#v", v)
	}
	if hb[0].Input["adopted"] != nil {
		t.Fatalf("process 1 adopted nothing, yet sent %#v", hb[0].Input["adopted"])
	}
}

// Restart (SIGHUP, `mesh node upgrade`) makes Run return ErrRestart at
// once while a build session runs — no drain, no wait — and the session
// is still alive afterwards; a new loop on the same root adopts it and
// finishes the item.
func TestRestartExitsAtOnceAndTheSessionIsAdopted(t *testing.T) {
	sessionunit.PollInterval = 20 * time.Millisecond
	t.Setenv("SLOW_EVENTS", "30")
	t.Setenv("SLOW_SLEEP", "0.1")
	f := seed(t)
	cfg := registeredNode(t, f)
	root := t.TempDir()
	claude := &countedClaude{ClaudeCode: &harness.ClaudeCode{Binary: slowClaudeScript(t), Unit: sessionunit.Detached{}}}

	loop1, _ := adoptNode(t, f, cfg, root, claude)
	ran1 := make(chan error, 1)
	go func() { ran1 <- loop1.Run(context.Background()) }()
	var found []FoundSession
	waitFor(t, func() bool {
		found, _ = FindSessions(root, []string{cfg.NodeID})
		return len(found) == 1 && found[0].State.Offset > 0
	})
	pid := found[0].State.PID
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	asked := time.Now()
	loop1.Restart()
	select {
	case err := <-ran1:
		if err != ErrRestart {
			t.Fatalf("Run = %v, want ErrRestart", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after Restart")
	}
	if took := time.Since(asked); took > time.Second {
		t.Fatalf("Run took %s to return", took)
	}
	if loop1.Draining() {
		t.Fatal("Restart drained")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("the session (pid %d) died with its node", pid)
	}
	if n := len(f.CallsNamed("finishWorkItem")); n != 0 {
		t.Fatalf("finished before the restart: %d", n)
	}

	loop2, _ := adoptNode(t, f, cfg, root, claude)
	found, err := FindSessions(root, []string{cfg.NodeID})
	if err != nil || len(found) != 1 || found[0].State.PID != pid || !found[0].Alive() {
		t.Fatalf("found %+v, %v", found, err)
	}
	ctx2, stop2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop2()
	if n := loop2.AdoptSessions(ctx2, found); n != 1 {
		t.Fatalf("adopted %d", n)
	}
	waitFor(t, func() bool { return f.Item(item1)["status"] == "evaluating" })
	loop2.wg.Wait()
	if n := len(f.CallsNamed("finishWorkItem")); n != 1 {
		t.Fatalf("finishWorkItem calls = %d, want 1", n)
	}
	if n := claude.starts.Load(); n != 1 {
		t.Fatalf("claude started %d times", n)
	}
}

// deadPID is the pid of a process that has ended.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// deadSession leaves a build session of item1 whose process is gone, its
// output as given, where the item's checkout is.
func deadSession(t *testing.T, root, nodeID string, output ...string) string {
	t.Helper()
	ws := WorkspaceSpec{Root: root, GoalSlug: "ship-it", Seq: 1}.Dir()
	dir := sessionunit.Dir(ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionunit.OutputFile), []byte(strings.Join(output, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := sessionunit.State{NodeID: nodeID, ItemID: item1, GoalID: goalID, Kind: "build", Runner: "detached", PID: deadPID(t), Workspace: ws, StartedAt: time.Now().Add(-time.Hour).UTC(), Offset: int64(len(output[0]) + 1)}
	if err := sessionunit.WriteState(dir, st); err != nil {
		t.Fatal(err)
	}
	return ws
}

// (b) A session whose process died while no node ran is reported from
// its output file: finished when it holds a result event, failed with
// its last text when it does not — and no session is started for it.
func TestAdoptedDeadSessionIsReportedFromItsOutput(t *testing.T) {
	const initLine = `{"type":"system","subtype":"init","session_id":"s-dead","model":"claude-sonnet-5"}`
	step := func(text string) string {
		return `{"type":"assistant","session_id":"s-dead","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}}`
	}
	for _, tc := range []struct {
		name   string
		output []string
		status string
	}{
		{"result", []string{initLine, step("wrote it"), `{"type":"result","subtype":"success","session_id":"s-dead","result":"all done","total_cost_usd":0.3,"num_turns":2}`}, "evaluating"},
		{"no result", []string{initLine, step("wrote it"), step("half way through the tests")}, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seed(t)
			cfg := registeredNode(t, f)
			root := t.TempDir()
			gw := gateway.New(f.URL(), "rs_test")
			if err := gw.ClaimWorkItem(context.Background(), item1, cfg.NodeID, "claude-code"); err != nil {
				t.Fatal(err)
			}
			ws := deadSession(t, root, cfg.NodeID, tc.output...)
			claude := &countedClaude{ClaudeCode: &harness.ClaudeCode{Binary: "/bin/false", Unit: sessionunit.Detached{}}}
			loop, _ := adoptNode(t, f, cfg, root, claude)
			found, err := FindSessions(root, []string{cfg.NodeID, "someone-else"})
			if err != nil || len(found) != 1 || found[0].Alive() {
				t.Fatalf("found %+v, %v", found, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			loop.AdoptSessions(ctx, found)
			loop.wg.Wait()
			row := f.Item(item1)
			if row["status"] != tc.status {
				t.Fatalf("status %v, want %s: %v", row["status"], tc.status, row)
			}
			switch tc.status {
			case "evaluating":
				if n := len(f.CallsNamed("finishWorkItem")); n != 1 || row["costCents"] != 30 {
					t.Fatalf("finish calls %d, row %v", n, row)
				}
			case "failed":
				if e, _ := row["lastError"].(string); !strings.Contains(e, "half way through the tests") || len(f.CallsNamed("finishWorkItem")) != 0 {
					t.Fatalf("lastError %q", e)
				}
			}
			if claude.starts.Load() != 0 || len(f.CallsNamed("claimWorkItem")) != 1 {
				t.Fatal("a dead session was run or claimed again")
			}
			if _, err := os.Stat(sessionunit.Dir(ws)); !os.IsNotExist(err) {
				t.Fatalf("session dir left: %v", err)
			}
			if loop.Running() != 0 || loop.Adopted() != nil {
				t.Fatalf("still running %d, adopted %v", loop.Running(), loop.Adopted())
			}
		})
	}
}

// A state file naming another node is not this node's to adopt.
func TestFindSessionsOnlyThisNodes(t *testing.T) {
	root := t.TempDir()
	deadSession(t, root, "other-node", `{"type":"system"}`)
	// a checkout is not searched inside
	if err := os.MkdirAll(filepath.Join(root, "g", "2", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	found, err := FindSessions(root, []string{"this-node"})
	if err != nil || len(found) != 0 {
		t.Fatalf("found %+v, %v", found, err)
	}
	if found, _ := FindSessions(root, []string{"other-node"}); len(found) != 1 {
		t.Fatalf("found %+v", found)
	}
	if found, err := FindSessions(filepath.Join(root, "missing"), []string{"x"}); err != nil || len(found) != 0 {
		t.Fatalf("missing root: %+v, %v", found, err)
	}
}
