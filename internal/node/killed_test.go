package node

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// leftovers is what an agent left running in its session's unit: the
// platform it started in the background for a check, and its shell.
var leftovers = []sessionunit.Process{{PID: 4242, Comm: "go"}, {PID: 4250, Comm: "runsheet"}}

// A build whose session's end killed what the agent left running logs
// one line naming them, and the item's note says the same sentence.
func TestBuildKilledLeftRunningIsLoggedAndNoted(t *testing.T) {
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		return harness.Result{Success: true, Text: "done", SessionID: "s-1", Killed: leftovers, KillError: "systemctl stop: exit status 1"}, nil
	})
	f := seed(t)
	r, _, item, row := runnerOnFake(t, f, h, nil)
	var logs bytes.Buffer
	r.Log = log.New(&logs, "", 0)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if row(item1)["status"] != "evaluating" {
		t.Fatalf("a kill's report is not the item's failure: %v", row(item1))
	}
	sentence := "session end killed 2 process(es) the agent left running: 4242 go, 4250 runsheet"
	if !strings.Contains(logs.String(), ": "+sentence+"\n") {
		t.Fatalf("log = %q", logs.String())
	}
	if !strings.Contains(logs.String(), ": kill: systemctl stop: exit status 1\n") {
		t.Fatalf("the kill's error is not logged: %q", logs.String())
	}
	var notes []string
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		if n, _ := c.Input["note"].(string); strings.Contains(n, "left running:") {
			notes = append(notes, n)
		}
	}
	if len(notes) != 1 || notes[0] != sentence {
		t.Fatalf("notes = %q", notes)
	}
}

// A handed-back build is two sessions: each one's end is its own line,
// and the note carries both.
func TestHandBackKilledLogsALinePerSession(t *testing.T) {
	second := []sessionunit.Process{{PID: 5252, Comm: "sleep"}}
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		return harness.Result{Success: true, Text: "done", SessionID: "s-1", HandedBack: true,
			Killed:     append(append([]sessionunit.Process(nil), leftovers...), second...),
			KilledRuns: [][]sessionunit.Process{leftovers, second}}, nil
	})
	f := seed(t)
	r, _, item, _ := runnerOnFake(t, f, h, nil)
	var logs bytes.Buffer
	r.Log = log.New(&logs, "", 0)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	first := "session end killed 2 process(es) the agent left running: 4242 go, 4250 runsheet"
	last := "session end killed 1 process(es) the agent left running: 5252 sleep"
	if n := strings.Count(logs.String(), "session end killed"); n != 2 {
		t.Fatalf("%d lines, want one per session: %q", n, logs.String())
	}
	if !strings.Contains(logs.String(), ": "+first+"\n") || !strings.Contains(logs.String(), ": "+last+"\n") {
		t.Fatalf("log = %q", logs.String())
	}
	var notes []string
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		if n, _ := c.Input["note"].(string); strings.Contains(n, "left running:") {
			notes = append(notes, n)
		}
	}
	if len(notes) != 1 || notes[0] != first+"\n"+last {
		t.Fatalf("notes = %q", notes)
	}
}

// A session that killed nothing says nothing of it.
func TestBuildKilledNothingLeftRunningSaysNothing(t *testing.T) {
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		return harness.Result{Success: true, Text: "done", SessionID: "s-1"}, nil
	})
	f := seed(t)
	r, _, item, _ := runnerOnFake(t, f, h, nil)
	var logs bytes.Buffer
	r.Log = log.New(&logs, "", 0)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "session end killed") || strings.Contains(logs.String(), "kill:") {
		t.Fatalf("log = %q", logs.String())
	}
	for _, c := range f.CallsNamed("reportWorkItemProgress") {
		if n, _ := c.Input["note"].(string); strings.Contains(n, "left running:") {
			t.Fatalf("note %q", n)
		}
	}
}

func TestKilledNote(t *testing.T) {
	if n := KilledNote(nil); n != "" {
		t.Fatalf("none = %q", n)
	}
	if n := KilledNote(leftovers[:1]); n != "session end killed 1 process(es) the agent left running: 4242 go" {
		t.Fatalf("one = %q", n)
	}
}

// killUnit is a unit whose Kill is recorded and reports canned
// leftovers.
type killUnit struct {
	mu     sync.Mutex
	kills  []sessionunit.Handle
	killed []sessionunit.Process
	err    error
}

func (u *killUnit) Name() string { return "fake" }
func (u *killUnit) Start(sessionunit.Spec) (sessionunit.Handle, error) {
	return sessionunit.Handle{}, nil
}
func (u *killUnit) Alive(sessionunit.Handle) bool   { return false }
func (u *killUnit) Stop(h sessionunit.Handle) error { _, err := u.Kill(h); return err }
func (u *killUnit) Kill(h sessionunit.Handle) ([]sessionunit.Process, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.kills = append(u.kills, h)
	return u.killed, u.err
}

// An adopted build whose item is terminal on the platform is not this
// node's: its unit is killed — claude long gone, what the agent left
// there not — the kill's report logged, and its directory removed.
func TestAdoptItemTerminalKillsLeftRunning(t *testing.T) {
	var logs bytes.Buffer
	r := &Runner{Log: log.New(&logs, "", 0)}
	u := &killUnit{killed: leftovers}
	dir := t.TempDir()
	s := FoundSession{Dir: dir + ".session", Unit: u, State: sessionunit.State{NodeID: "n-1", ItemID: item1, Kind: "build", PID: 777, Unit: "mesh-session-build-x-1", Workspace: dir}}
	item := gateway.WorkItem{ID: item1, Status: "merged", NodeID: "n-1"}
	if err := r.AdoptItem(context.Background(), item, s); err != nil {
		t.Fatal(err)
	}
	if len(u.kills) != 1 || u.kills[0] != (sessionunit.Handle{PID: 777, UnitName: "mesh-session-build-x-1"}) {
		t.Fatalf("kills = %v", u.kills)
	}
	want := "item " + item1 + ": session end killed 2 process(es) the agent left running: 4242 go, 4250 runsheet\n"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log = %q", logs.String())
	}
}

// An evaluation that ran out of time records no verdict, and the
// platform takes no note on an item in evaluating: what its end killed
// is logged, and nothing is sent as the item's progress.
func TestEvaluateTimeoutKilledIsLogged(t *testing.T) {
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		if job.Kind != harness.KindEvaluate {
			return harness.Result{Success: true, Text: "done", SessionID: "s-1"}, nil
		}
		return harness.Result{SessionID: "s-2", Killed: leftovers}, context.DeadlineExceeded
	})
	f := seed(t)
	r, ws, item, row := runnerOnFake(t, f, h, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	progress := len(f.CallsNamed("reportWorkItemProgress"))
	var logs bytes.Buffer
	ev := &Evaluator{GW: r.GW, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, Log: log.New(&logs, "", 0)}
	item.Branch = "mesh/ship-it-1"
	if err := ev.Evaluate(context.Background(), item); err == nil || !strings.Contains(err.Error(), "ran out of time") {
		t.Fatalf("err = %v", err)
	}
	if row(item1)["status"] != "evaluating" {
		t.Fatalf("a timed-out evaluation records no verdict: %v", row(item1))
	}
	want := "item ship-it/1: session end killed 2 process(es) the agent left running: 4242 go, 4250 runsheet\n"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log = %q", logs.String())
	}
	if n := len(f.CallsNamed("reportWorkItemProgress")); n != progress {
		t.Fatalf("the evaluation sent %d progress notes; the platform takes none on an item in evaluating", n-progress)
	}
	if n := len(f.CallsNamed("evaluateWorkItem")); n != 0 {
		t.Fatalf("verdicts = %d", n)
	}
}

// An evaluation that records a verdict carries what its end killed in
// the verdict's notes — and, held by LeftRunning, what an earlier
// evaluation of the item that recorded none killed.
func TestEvaluateVerdictCarriesKilled(t *testing.T) {
	second := []sessionunit.Process{{PID: 5252, Comm: "sleep"}}
	timedOut := true
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		if job.Kind != harness.KindEvaluate {
			return harness.Result{Success: true, Text: "done", SessionID: "s-1"}, nil
		}
		if timedOut {
			return harness.Result{SessionID: "s-2", Killed: leftovers}, context.DeadlineExceeded
		}
		return harness.Result{Success: true, Text: "Fine.\nVERDICT: PASS", SessionID: "s-3", Killed: second}, nil
	})
	f := seed(t)
	r, ws, item, _ := runnerOnFake(t, f, h, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	left := &LeftRunning{}
	ev := &Evaluator{GW: r.GW, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, LeftRunning: left, Log: log.New(&logs, "", 0)}
	item.Branch = "mesh/ship-it-1"
	if err := ev.Evaluate(context.Background(), item); err == nil {
		t.Fatal("a timed-out evaluation records no verdict")
	}
	// the next evaluation, a later tick's, ends with a verdict
	timedOut = false
	if err := ev.Evaluate(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	earlier := "session end killed 2 process(es) the agent left running: 4242 go, 4250 runsheet"
	own := "session end killed 1 process(es) the agent left running: 5252 sleep"
	if !strings.Contains(logs.String(), "item ship-it/1: "+own+"\n") {
		t.Fatalf("log = %q", logs.String())
	}
	calls := f.CallsNamed("evaluateWorkItem")
	if len(calls) != 1 {
		t.Fatalf("verdicts = %d", len(calls))
	}
	if n, _ := calls[0].Input["notes"].(string); !strings.HasSuffix(n, "\n"+earlier+"\n"+own) {
		t.Fatalf("verdict notes = %q", n)
	}
	if held := left.Hold(item1, ""); held != "" {
		t.Fatalf("still held after the verdict: %q", held)
	}
}

// An evaluation whose own deadline passed — the ctx the node ran it on
// expired, not only the harness's error — still logs what its end
// killed before it returns: a deadline kills the unit, it is no detach.
func TestEvaluateExpiredContextKilledIsLogged(t *testing.T) {
	h := harnessFunc(func(ctx context.Context, job harness.Job, on func(harness.Progress)) (harness.Result, error) {
		if job.Kind != harness.KindEvaluate {
			return harness.Result{Success: true, Text: "done", SessionID: "s-1"}, nil
		}
		<-ctx.Done()
		return harness.Result{SessionID: "s-2", Killed: leftovers}, ctx.Err()
	})
	f := seed(t)
	r, ws, item, row := runnerOnFake(t, f, h, nil)
	if err := r.RunItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	progress := len(f.CallsNamed("reportWorkItemProgress"))
	var logs bytes.Buffer
	ev := &Evaluator{GW: r.GW, Harness: h, WS: ws, Scopes: r.Scopes, WorkRoot: ws.root, Log: log.New(&logs, "", 0)}
	item.Branch = "mesh/ship-it-1"
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := ev.Evaluate(ctx, item); err == nil {
		t.Fatal("an evaluation whose ctx expired records no verdict")
	}
	if ctx.Err() == nil {
		t.Fatal("the ctx did not expire")
	}
	if row(item1)["status"] != "evaluating" {
		t.Fatalf("status = %v", row(item1))
	}
	want := "item ship-it/1: session end killed 2 process(es) the agent left running: 4242 go, 4250 runsheet\n"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log = %q", logs.String())
	}
	if n := len(f.CallsNamed("reportWorkItemProgress")); n != progress {
		t.Fatalf("the evaluation sent %d progress notes", n-progress)
	}
}

// A plan and a research session whose ctx expired log what their end
// killed before they return.
func TestPlanAndResearchExpiredContextKilledIsLogged(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	result := harness.Result{SessionID: "s-1", Killed: leftovers}
	line := ": session end killed 2 process(es) the agent left running: 4242 go, 4250 runsheet\n"

	var logs bytes.Buffer
	p := &Planner{Log: log.New(&logs, "", 0)}
	if _, _, _, _, _, err := p.readPlan(ctx, gateway.Goal{Slug: "ship-it"}, t.TempDir(), nil, result, context.DeadlineExceeded, nil, nil, nil); err == nil {
		t.Fatal("an expired plan hands in nothing")
	}
	if !strings.Contains(logs.String(), "goal ship-it"+line) {
		t.Fatalf("plan log = %q", logs.String())
	}

	logs.Reset()
	d := &Director{Log: log.New(&logs, "", 0)}
	if err := d.handIn(ctx, gateway.Direction{Slug: "nightly"}, gateway.Project{Slug: "mesh"}, Scope{}, t.TempDir(), nil, result, context.DeadlineExceeded); err == nil {
		t.Fatal("an expired research hands in nothing")
	}
	if !strings.Contains(logs.String(), "mesh/nightly"+line) {
		t.Fatalf("research log = %q", logs.String())
	}
}
