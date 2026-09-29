package node

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"

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
