package actor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

func readRecord(t *testing.T, dir string) Record {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// actor.json is written before each state change: what is on disk is
// always the state the actor is in.
func TestActorJSONIsWrittenBeforeEachChange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lse_1")
	a, err := Start(dir, Record{LeaseID: "lse_1", JobID: "job_1", Kind: "conform/echo"})
	if err != nil {
		t.Fatal(err)
	}
	var seen []State
	a.Logf = func(string, ...any) { seen = append(seen, readRecord(t, dir).State) }
	if r := readRecord(t, dir); r.State != Taken || r.PID != os.Getpid() || r.JobID != "job_1" {
		t.Fatalf("actor.json %+v", r)
	}
	for _, s := range []State{Preparing, Running, Reporting} {
		if err := a.Enter(s); err != nil {
			t.Fatal(err)
		}
	}
	if want := []State{Preparing, Running, Reporting}; fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("on disk at each change %v, want %v", seen, want)
	}
}

// The states allow only their own moves; releasing is reachable from
// anywhere and is the only way to done.
func TestMoves(t *testing.T) {
	for _, tc := range []struct {
		path []State
		ok   bool
	}{
		{[]State{Preparing, Running, Reporting, Releasing, Done}, true},
		{[]State{Releasing, Done}, true},
		{[]State{Preparing, Releasing, Done}, true},
		{[]State{Running}, false},
		{[]State{Preparing, Running, Done}, false},
		{[]State{Preparing, Running, Reporting, Running}, false},
		{[]State{Releasing, Releasing}, false},
	} {
		a, err := Start(t.TempDir(), Record{LeaseID: "lse"})
		if err != nil {
			t.Fatal(err)
		}
		var got error
		for _, s := range tc.path {
			if got = a.Enter(s); got != nil {
				break
			}
		}
		if (got == nil) != tc.ok || (got != nil && !errors.Is(got, ErrMove)) {
			t.Errorf("%v: %v", tc.path, got)
		}
	}
}

// Cancelling is reachable from any state before reporting, keeps the
// first reason, and leads on to reporting.
func TestCancel(t *testing.T) {
	a, _ := Start(t.TempDir(), Record{LeaseID: "lse"})
	a.Enter(Preparing)
	if !a.Cancel("Mesh cancelled the lease") || a.Cancel("again") {
		t.Fatal("cancel moved twice")
	}
	if a.State() != Cancelling || a.Cancelled() != "Mesh cancelled the lease" {
		t.Fatalf("%s %q", a.State(), a.Cancelled())
	}
	if err := a.Enter(Reporting); err != nil {
		t.Fatal(err)
	}
	if a.Cancel("late") {
		t.Fatal("a reporting actor was cancelled")
	}
}

// Release releases what the actor holds in reverse order — one that
// fails or panics does not stop the rest — then is done, and the job
// directory is gone. Releasing twice is nothing.
func TestReleaseIsTheOnlyExit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lse_1")
	a, _ := Start(dir, Record{LeaseID: "lse_1"})
	var order []string
	a.Hold("room", func() error { order = append(order, "room"); return nil })
	a.Hold("grant", func() error { order = append(order, "grant"); return errors.New("revoke failed") })
	a.HoldPod("mesh-v1-lse_1", func() error { order = append(order, "pod"); panic("boom") })
	a.Hold("proxy", func() error { order = append(order, "proxy"); return nil })
	if r := readRecord(t, dir); fmt.Sprint(r.Holds) != "[room grant pod mesh-v1-lse_1 proxy]" || fmt.Sprint(r.Pods) != "[mesh-v1-lse_1]" {
		t.Fatalf("actor.json holds %v pods %v", r.Holds, r.Pods)
	}
	a.Enter(Preparing)
	a.Release()
	a.Release()
	if fmt.Sprint(order) != "[proxy pod grant room]" {
		t.Fatalf("released %v", order)
	}
	if a.State() != Done {
		t.Fatalf("state %s", a.State())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the job directory is still there: %v", err)
	}
}

// A node that stopped leaves its actors' records; the next start reads
// every one that is not done.
func TestLeftovers(t *testing.T) {
	root := t.TempDir()
	a, _ := Start(filepath.Join(root, "lse_1"), Record{LeaseID: "lse_1"})
	a.Enter(Preparing)
	a.Enter(Running)
	b, _ := Start(filepath.Join(root, "lse_2"), Record{LeaseID: "lse_2"})
	b.Release()
	os.MkdirAll(filepath.Join(root, "not-an-actor"), 0o700)
	got, err := Leftovers(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Record.LeaseID != "lse_1" || got[0].Record.State != Running {
		t.Fatalf("leftovers %+v", got)
	}
	if none, err := Leftovers(filepath.Join(root, "missing")); err != nil || none != nil {
		t.Fatalf("a missing root: %v %v", none, err)
	}
}

// Killed reads an end by SIGTERM or SIGKILL however it comes: a
// container's exit, a process's signal, or a session's error text.
func TestKilled(t *testing.T) {
	sigkill := exec.Command("sh", "-c", "kill -KILL $$").Run()
	exit3 := exec.Command("sh", "-c", "exit 3").Run()
	for _, tc := range []struct {
		name string
		err  error
		code int
		ok   bool
	}{
		{"nil", nil, 0, false},
		{"container 137", &sandbox.ExitError{Code: 137}, 137, true},
		{"container 143", fmt.Errorf("agent: %w", &sandbox.ExitError{Code: 143}), 143, true},
		{"container 1", &sandbox.ExitError{Code: 1}, 1, false},
		{"process SIGKILL", sigkill, 137, true},
		{"process exit 3", exit3, 3, false},
		{"session text", errors.New("claude: signal: killed"), 137, true},
		{"session text 143", errors.New("claude: exit status 143"), 143, true},
		{"other", errors.New("claude: not logged in"), 0, false},
	} {
		code, ok := Killed(tc.err)
		if code != tc.code || ok != tc.ok {
			t.Errorf("%s: Killed = %d %v, want %d %v", tc.name, code, ok, tc.code, tc.ok)
		}
	}
}

// ReleaseNow releases the named holds at once and leaves the rest for
// Release.
func TestReleaseNow(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lse")
	a, _ := Start(dir, Record{LeaseID: "lse"})
	var order []string
	for _, n := range []string{"room", "grant", "proxy"} {
		a.Hold(n, func() error { order = append(order, n); return nil })
	}
	a.ReleaseNow("grant", "proxy")
	if fmt.Sprint(order) != "[proxy grant]" || fmt.Sprint(readRecord(t, dir).Holds) != "[room]" {
		t.Fatalf("released %v, actor.json holds %v", order, readRecord(t, dir).Holds)
	}
	a.Release()
	if fmt.Sprint(order) != "[proxy grant room]" {
		t.Fatalf("released %v", order)
	}
}
