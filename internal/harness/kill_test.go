//go:build !windows

package harness

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// killUnit is a sessionunit.Unit that runs sessions detached, as a
// real unit would, and whose Kill ends the session's process, records
// the call and reports canned leftovers: the n-th call's are the
// leftovers with their PIDs raised by 1000×(n-1), so two runs' are told
// apart.
type killUnit struct {
	sessionunit.Detached
	leftovers []sessionunit.Process
	err       error

	mu    sync.Mutex
	calls []sessionunit.Handle
}

func (u *killUnit) Name() string { return "kill-fake" }

func (u *killUnit) Kill(h sessionunit.Handle) ([]sessionunit.Process, error) {
	_ = u.Detached.Stop(h)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, h)
	var out []sessionunit.Process
	for _, p := range u.leftovers {
		p.PID += 1000 * (len(u.calls) - 1)
		out = append(out, p)
	}
	return out, u.err
}

func (u *killUnit) killCalls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

// leftovers is what the agent in the mini's leak left running.
var leftovers = []sessionunit.Process{
	{PID: 4242, Comm: "go", Cmdline: "go run ./cmd/runsheet -addr :18211"},
	{PID: 4250, Comm: "bash", Cmdline: "bash -c go run ./cmd/runsheet -addr :18211 &"},
}

// scriptClaude writes a claude stand-in running script into a temp dir.
func scriptClaude(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\ncat >/dev/null\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertKilledOnce(t *testing.T, u *killUnit, res Result) {
	t.Helper()
	if n := u.killCalls(); n != 1 {
		t.Fatalf("Kill called %d times, want once", n)
	}
	if !reflect.DeepEqual(res.Killed, leftovers) {
		t.Fatalf("Result.Killed = %v, want %v", res.Killed, leftovers)
	}
}

// A session that ends on its own kills its unit once, and the Result
// names what the agent left running there.
func TestKillOnFinish(t *testing.T) {
	u := &killUnit{leftovers: leftovers}
	h := &ClaudeCode{Binary: slowClaude(t), Unit: u}
	res, err := h.Run(context.Background(), Job{Kind: KindBuild, Workspace: t.TempDir(), Env: []string{"SLOW_EVENTS=2", "SLOW_SLEEP=0.05"}}, nil)
	if err != nil || !res.Success || res.Text != "all done" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	assertKilledOnce(t, u, res)
}

// A non-zero exit is killed the same, and its error is as before.
func TestKillOnNonZeroExit(t *testing.T) {
	u := &killUnit{leftovers: leftovers}
	h := &ClaudeCode{Binary: scriptClaude(t, "echo 'boom' >&2\nexit 2\n"), Unit: u}
	res, err := h.Run(context.Background(), Job{Kind: KindBuild, Workspace: t.TempDir()}, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the stderr's boom", err)
	}
	assertKilledOnce(t, u, res)
}

// A session that says it is unavailable is killed, and still an
// UnavailableError.
func TestKillOnUnavailable(t *testing.T) {
	u := &killUnit{leftovers: leftovers}
	h := &ClaudeCode{Binary: scriptClaude(t, "echo usage limit reached >&2\nexit 1\n"), Unit: u}
	res, err := h.Run(context.Background(), Job{Kind: KindBuild, Workspace: t.TempDir()}, nil)
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want an UnavailableError", err)
	}
	assertKilledOnce(t, u, res)
}

// A cancelled ctx kills the unit once — the stop is the kill — and Run
// still returns ctx.Err().
func TestKillOnCancel(t *testing.T) {
	u := &killUnit{leftovers: leftovers}
	h := &ClaudeCode{Binary: slowClaude(t), Unit: u}
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	res, err := h.Run(ctx, Job{Kind: KindBuild, Workspace: t.TempDir(), Env: []string{"SLOW_EVENTS=100", "SLOW_SLEEP=0.1"}}, func(Progress) { once.Do(cancel) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
	assertKilledOnce(t, u, res)
}

// The job's Timeout kills the unit once, and Run returns the deadline.
func TestKillOnTimeout(t *testing.T) {
	u := &killUnit{leftovers: leftovers}
	h := &ClaudeCode{Binary: slowClaude(t), Unit: u}
	res, err := h.Run(context.Background(), Job{Kind: KindBuild, Workspace: t.TempDir(), Timeout: 300 * time.Millisecond, Env: []string{"SLOW_EVENTS=100", "SLOW_SLEEP=0.1"}}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	assertKilledOnce(t, u, res)
}

// A session that outlives a node shutting down is left for the next
// node: its unit is not killed.
func TestKillNotOnDetach(t *testing.T) {
	u := &killUnit{leftovers: leftovers}
	h := &ClaudeCode{Binary: slowClaude(t), Unit: u}
	ws := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	res, err := h.Run(ctx, Job{Kind: KindBuild, Workspace: ws, Outlive: true, Env: []string{"SLOW_EVENTS=100", "SLOW_SLEEP=0.1"}}, func(Progress) { once.Do(cancel) })
	st, _ := sessionunit.ReadState(sessionunit.Dir(ws))
	t.Cleanup(func() { _ = sessionunit.Detached{}.Stop(sessionunit.Handle{PID: st.PID}) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
	if n := u.killCalls(); n != 0 {
		t.Fatalf("Kill called %d times on a detached session", n)
	}
	if res.Killed != nil {
		t.Fatalf("Result.Killed = %v on a detached session", res.Killed)
	}
	if st.PID == 0 || !alive(st.PID) {
		t.Fatalf("detached session %d is not running", st.PID)
	}
}

// An adopted session is killed once when it ends, and the Result names
// its leftovers.
func TestKillOnAttach(t *testing.T) {
	u := &killUnit{leftovers: leftovers}
	h := &ClaudeCode{Binary: slowClaude(t), Unit: u}
	ws := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	_, err := h.Run(ctx, Job{Kind: KindBuild, Workspace: ws, Outlive: true, Env: []string{"SLOW_EVENTS=5", "SLOW_SLEEP=0.1"}}, func(Progress) { once.Do(cancel) })
	if !errors.Is(err, context.Canceled) || u.killCalls() != 0 {
		t.Fatalf("Run = %v with %d kills, want a detach", err, u.killCalls())
	}
	res, err := h.Attach(context.Background(), sessionunit.Dir(ws), nil)
	if err != nil || !res.Success {
		t.Fatalf("Attach = %+v, %v", res, err)
	}
	assertKilledOnce(t, u, res)
}

// A handed-back session's unit is killed at the hand-back's stop and
// again at the resumed run's end; the Result's Killed is both runs'.
func TestKillOnHandBack(t *testing.T) {
	bin, err := filepath.Abs("testdata/handback_claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	u := &killUnit{leftovers: leftovers}
	h := &ClaudeCode{Binary: bin, Unit: u}
	res, err := h.Run(context.Background(), Job{
		Kind: KindBuild, Workspace: t.TempDir(), Prompt: "build it", Model: "sonnet",
		MaxBudgetUSD: 4, HandBackAt: DefaultHandBackAt, Timeout: time.Minute,
		HasCommit: func() bool { return false },
		Env:       []string{"HB_RECORD=" + filepath.Join(t.TempDir(), "record"), "HB_TURNS=3", "HB_INPUT=500000", "HB_SLEEP=30"},
	}, nil)
	if err != nil || !res.HandedBack {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if n := u.killCalls(); n != 2 {
		t.Fatalf("Kill called %d times, want once per run", n)
	}
	want := append([]sessionunit.Process(nil), leftovers...)
	for _, p := range leftovers {
		p.PID += 1000
		want = append(want, p)
	}
	if !reflect.DeepEqual(res.Killed, want) {
		t.Fatalf("Result.Killed = %v, want %v", res.Killed, want)
	}
}

// A kill that fails is said on Stderr and never fails the session.
func TestKillErrorToStderr(t *testing.T) {
	var stderr bytes.Buffer
	u := &killUnit{err: errors.New("systemctl stop: exit status 1")}
	h := &ClaudeCode{Binary: slowClaude(t), Unit: u, Stderr: &stderr}
	res, err := h.Run(context.Background(), Job{Kind: KindBuild, Workspace: t.TempDir(), Env: []string{"SLOW_EVENTS=1", "SLOW_SLEEP=0.01"}}, nil)
	if err != nil || !res.Success {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if !strings.Contains(stderr.String(), "harness claude-code: kill session") || !strings.Contains(stderr.String(), "systemctl stop: exit status 1") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if res.KillError != "" {
		t.Fatalf("KillError = %q with a Stderr", res.KillError)
	}
}

// With no Stderr the kill's error is on the Result, and the session
// still succeeds.
func TestKillErrorOnResult(t *testing.T) {
	u := &killUnit{err: errors.New("systemctl stop: exit status 1")}
	h := &ClaudeCode{Binary: slowClaude(t), Unit: u}
	res, err := h.Run(context.Background(), Job{Kind: KindBuild, Workspace: t.TempDir(), Env: []string{"SLOW_EVENTS=1", "SLOW_SLEEP=0.01"}}, nil)
	if err != nil || !res.Success {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if !strings.Contains(res.KillError, "systemctl stop: exit status 1") {
		t.Fatalf("KillError = %q", res.KillError)
	}
}
