//go:build !windows

package node

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// A build whose item the node no longer holds is stopped (goal
// a-stalled-session-is-stopped-not-orphaned, item 1): for each way the
// platform gives a build up — a stall putting it back in ready, its
// lease ending and another node taking it, a release, a cancel — the
// session's unit is killed within one ProgressEvery and the unit's stop
// grace, the node's slot is freed, and nothing is sent for the item.

const leaseHolder = "33333333-3333-4333-8333-333333333333"

// lostCase is one way the platform gives a build up: the item's row
// after it, and the reason the node's log names.
type lostCase struct {
	name   string
	fields map[string]any
	why    string
}

var lostCases = []lostCase{
	{"stall", map[string]any{"status": "ready", "nodeId": nil, "lastError": "stalled: no heartbeat for 45 minutes"}, "item is ready"},
	{"lease end", map[string]any{"status": "claimed", "nodeId": leaseHolder}, "item is claimed on node " + leaseHolder},
	{"release", map[string]any{"status": "ready", "nodeId": nil}, "item is ready"},
	{"cancel", map[string]any{"status": "cancelled"}, "item is cancelled"},
}

// stopUnit is a real detached unit whose Stop and Kill are recorded with
// when they came. Kill is the unit's stop — SIGTERM, then SIGKILL after
// sessionunit.StopGrace, waiting until the group is gone — with the
// report of what it held; Stop is Kill without the report.
type stopUnit struct {
	sessionunit.Detached
	mu    sync.Mutex
	stops []time.Time
	kills []time.Time
}

func (u *stopUnit) Name() string { return "lost-fake" }

func (u *stopUnit) Stop(h sessionunit.Handle) error {
	u.mu.Lock()
	u.stops = append(u.stops, time.Now())
	u.mu.Unlock()
	return u.Detached.Stop(h)
}

func (u *stopUnit) Kill(h sessionunit.Handle) ([]sessionunit.Process, error) {
	u.mu.Lock()
	u.kills = append(u.kills, time.Now())
	u.mu.Unlock()
	return u.Detached.Kill(h)
}

// firstStop is when the unit was first stopped (by Stop or Kill), zero
// when never.
func (u *stopUnit) firstStop() time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	var first time.Time
	for _, at := range append(append([]time.Time(nil), u.stops...), u.kills...) {
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
	return first
}

func (u *stopUnit) killCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.kills)
}

// lostBound is how long after the item is given up its session may run:
// one ProgressEvery, the unit's stop grace, and a second for the round
// trips to the fake.
func lostBound(every time.Duration) time.Duration {
	return every + sessionunit.StopGrace + time.Second
}

// fastFollow makes the session's output read without the default poll's
// delay.
func fastFollow(t *testing.T) {
	was := sessionunit.PollInterval
	sessionunit.PollInterval = 20 * time.Millisecond
	t.Cleanup(func() { sessionunit.PollInterval = was })
}

// pidGone waits for pid to be gone (or a zombie) within d.
func pidGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		if st, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			if i := strings.LastIndexByte(string(st), ')'); i >= 0 && i+2 < len(st) && st[i+2] == 'Z' {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// assertNothingSent: no fail, finish or release of any kind was sent
// for the item.
func assertNothingSent(t *testing.T, calls func(string) int) {
	t.Helper()
	for _, m := range []string{"failWorkItem", "finishWorkItem", "releaseWorkItem"} {
		if n := calls(m); n != 0 {
			t.Fatalf("%s sent %d time(s) for an item the node no longer holds", m, n)
		}
	}
}

// runningBuildSession waits for the build's session under root to be running
// and to have read its output, and returns it.
func runningBuildSession(t *testing.T, root, nodeID string) FoundSession {
	t.Helper()
	var found []FoundSession
	waitFor(t, func() bool {
		found, _ = FindSessions(root, []string{nodeID})
		return len(found) == 1 && found[0].State.Offset > 0
	})
	pid := found[0].State.PID
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return found[0]
}

// readsFail fails the item read once armed: the lost-claim check then
// has the heartbeat's refusal alone to go on.
type readsFail struct {
	Gateway
	armed atomic.Bool
}

func (g *readsFail) WorkItem(ctx context.Context, id string) (*gateway.WorkItem, error) {
	if g.armed.Load() {
		return nil, errors.New("read: connection reset")
	}
	return g.Gateway.WorkItem(ctx, id)
}

// runLostHost runs item 1's build on the real Claude Code harness in a
// recording detached unit, gives the item up as c says once the session
// runs, and checks the session was stopped in time and nothing sent.
// refusalOnly fails the item read, so the heartbeat's refusal is what
// says the claim was lost.
func runLostHost(t *testing.T, c lostCase, refusalOnly bool) {
	fastFollow(t)
	t.Setenv("SLOW_EVENTS", "1000")
	t.Setenv("SLOW_SLEEP", "0.05")
	f := seed(t)
	u := &stopUnit{}
	h := &harness.ClaudeCode{Binary: slowClaudeScript(t), Unit: u}
	var rf *readsFail
	var wrap func(Gateway) Gateway
	if refusalOnly {
		wrap = func(g Gateway) Gateway { rf = &readsFail{Gateway: g}; return rf }
	}
	r, ws, item, _ := runnerOnFake(t, f, h, wrap)
	r.ProgressEvery = 100 * time.Millisecond
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	done := make(chan error, 1)
	go func() { done <- r.RunItem(context.Background(), item) }()

	s := runningBuildSession(t, ws.root, r.Cfg.NodeID)
	if rf != nil {
		rf.armed.Store(true)
	}
	lostAt := time.Now()
	f.SetItem(item1, c.fields)
	bound := lostBound(r.ProgressEvery)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunItem = %v; a lost claim is no error of the node's", err)
		}
	case <-time.After(bound + 2*time.Second):
		t.Fatalf("RunItem still running %v after the claim was lost", bound+2*time.Second)
	}
	at := u.firstStop()
	if at.IsZero() || u.killCount() == 0 {
		t.Fatal("the session's unit was never stopped and killed")
	}
	if d := at.Sub(lostAt); d > bound {
		t.Fatalf("unit stopped %v after the claim was lost, want within %v", d, bound)
	}
	if !pidGone(s.State.PID, time.Second) {
		t.Fatalf("the session (pid %d) still runs", s.State.PID)
	}
	if _, err := os.Stat(sessionunit.Dir(s.State.Workspace)); !os.IsNotExist(err) {
		t.Fatalf("session dir left: %v", err)
	}
	assertNothingSent(t, func(m string) int { return len(f.CallsNamed(m)) })
	want := "item ship-it/1: claim lost (" + c.why
	if refusalOnly {
		want = "item ship-it/1: claim lost (progress refused: "
	}
	if !strings.Contains(logs.String(), want) || !strings.Contains(logs.String(), "); session stopped\n") {
		t.Fatalf("log = %q, want %q … session stopped", logs.String(), want)
	}
}

// (1) and (3): the item read back says the claim is lost.
func TestLostClaimStopsHostBuild(t *testing.T) {
	for _, c := range lostCases {
		t.Run(c.name, func(t *testing.T) { runLostHost(t, c, false) })
	}
}

// (1) and (3): the item read fails, and the heartbeat's refusal — `item
// is ready, not running`, `item is cancelled, not running` — is what
// says the claim is lost.
func TestLostClaimByRefusedHeartbeat(t *testing.T) {
	for _, c := range lostCases {
		if c.name == "lease end" {
			continue // the fake takes another node's heartbeat
		}
		t.Run(c.name, func(t *testing.T) { runLostHost(t, c, true) })
	}
}

// (1) and (3) for a sandboxed build: its pod and HOME are removed once
// the claim is lost, and nothing is sent.
func TestLostClaimClosesSandboxPod(t *testing.T) {
	for _, c := range lostCases {
		t.Run(c.name, func(t *testing.T) {
			rt := &sandbox.Fake{}
			started := make(chan struct{})
			rt.Agent = func(ctx context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
				_, _ = io.Copy(io.Discard, spec.Stdin)
				close(started)
				<-ctx.Done()
				return sandbox.Exit{Code: 137}, nil
			}
			r, sb, item, f := teardownRunner(t, rt, true, servicesYML)
			r.ProgressEvery = 100 * time.Millisecond
			logs := &syncBuf{}
			r.Log = log.New(logs, "", 0)
			done := make(chan error, 1)
			go func() { done <- r.RunItem(context.Background(), item) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatalf("the agent never started: %v", rt.Calls)
			}
			f.SetItem(item1, c.fields)
			bound := lostBound(r.ProgressEvery)
			select {
			case <-done:
			case <-time.After(bound):
				t.Fatalf("RunItem still running %v after the claim was lost", bound)
			}
			assertTornDown(t, rt, sb)
			assertNothingSent(t, func(m string) int { return len(f.CallsNamed(m)) })
			if !strings.Contains(logs.String(), "item ship-it/1: claim lost ("+c.why) {
				t.Fatalf("log = %q", logs.String())
			}
		})
	}
}

// (2): the loop's slot is freed — Running() drops by one — and the next
// tick claims a ready item into it, for each way the claim is lost.
func TestLostClaimFreesTheSlot(t *testing.T) {
	for _, c := range lostCases {
		t.Run(c.name, func(t *testing.T) {
			fastFollow(t)
			t.Setenv("SLOW_EVENTS", "1000")
			t.Setenv("SLOW_SLEEP", "0.05")
			f := seed(t)
			cfg := registeredNode(t, f) // capacity 1
			root := t.TempDir()
			u := &stopUnit{}
			claude := &harness.ClaudeCode{Binary: slowClaudeScript(t), Unit: u}
			loop, _ := adoptNode(t, f, cfg, root, claude)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			loop.Tick(ctx)
			runningBuildSession(t, root, cfg.NodeID)
			if n := loop.Running(); n != 1 {
				t.Fatalf("Running() = %d, want 1", n)
			}
			// item 2 is ready, but the one slot is taken
			f.SetItem(item2, map[string]any{"status": "ready", "dependsOn": []string{}})
			loop.Tick(ctx)
			if s := f.Item(item2)["status"]; s != "ready" {
				t.Fatalf("item 2 claimed past capacity: %v", s)
			}

			f.SetItem(item1, c.fields)
			deadline := time.Now().Add(lostBound(50 * time.Millisecond))
			for loop.Running() != 0 {
				if time.Now().After(deadline) {
					t.Fatalf("Running() = %d after the claim was lost", loop.Running())
				}
				time.Sleep(10 * time.Millisecond)
			}
			if u.killCount() == 0 {
				t.Fatal("the session's unit was never killed")
			}
			// the freed slot is claimed on the next tick: item 2, or item 1
			// again where the platform put it back in ready
			loop.Tick(ctx)
			var claimed string
			for _, id := range []string{item1, item2} {
				if row := f.Item(id); (row["status"] == "claimed" || row["status"] == "running") && row["nodeId"] == cfg.NodeID {
					claimed = id
				}
			}
			if claimed == "" || loop.Running() != 1 {
				t.Fatalf("the freed slot was not claimed on the next tick: Running() = %d, item 1 %v, item 2 %v", loop.Running(), f.Item(item1)["status"], f.Item(item2)["status"])
			}
			assertNothingSent(t, func(m string) int { return len(f.CallsNamed(m)) })
			// the new session ends the same way, so nothing is left
			runningBuildSession(t, root, cfg.NodeID)
			f.SetItem(claimed, map[string]any{"status": "cancelled"})
			loop.wg.Wait()
			assertNothingSent(t, func(m string) int { return len(f.CallsNamed(m)) })
		})
	}
}

// An adopted build is covered the same way: the session the process
// before left running is taken up, its item cancelled, and its unit is
// killed, the slot freed and nothing sent.
func TestLostClaimStopsAdoptedBuild(t *testing.T) {
	fastFollow(t)
	t.Setenv("SLOW_EVENTS", "1000")
	t.Setenv("SLOW_SLEEP", "0.05")
	f := seed(t)
	cfg := registeredNode(t, f)
	root := t.TempDir()
	u := &stopUnit{}
	claude := &harness.ClaudeCode{Binary: slowClaudeScript(t), Unit: u}

	// process 1 claims and starts the session, then stops: it detaches
	loop1, _ := adoptNode(t, f, cfg, root, claude)
	ctx1, stop1 := context.WithCancel(context.Background())
	ran1 := make(chan error, 1)
	go func() { ran1 <- loop1.Run(ctx1) }()
	s := runningBuildSession(t, root, cfg.NodeID)
	stop1()
	select {
	case <-ran1:
	case <-time.After(5 * time.Second):
		t.Fatal("loop 1's Run did not return")
	}
	if u.killCount() != 0 {
		t.Fatal("a shutdown killed the session; it must detach")
	}

	// process 2 adopts it
	loop2, runner2 := adoptNode(t, f, cfg, root, claude)
	logs := &syncBuf{}
	runner2.Log = log.New(logs, "", 0)
	found, err := FindSessions(root, []string{cfg.NodeID})
	if err != nil || len(found) != 1 {
		t.Fatalf("found %v, %v", found, err)
	}
	ctx2, stop2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop2()
	if n := loop2.AdoptSessions(ctx2, found); n != 1 || loop2.Running() != 1 {
		t.Fatalf("adopted %d, Running() = %d", n, loop2.Running())
	}
	// the adopter's first heartbeat has gone; the item is then cancelled
	waitFor(t, func() bool { return strings.Contains(logs.String(), "adopted its running session") })
	lostAt := time.Now()
	f.SetItem(item1, map[string]any{"status": "cancelled"})
	bound := lostBound(runner2.ProgressEvery)
	for loop2.Running() != 0 {
		if time.Since(lostAt) > bound+time.Second {
			t.Fatalf("Running() = %d after the claim was lost", loop2.Running())
		}
		time.Sleep(10 * time.Millisecond)
	}
	loop2.wg.Wait()
	at := u.firstStop()
	if at.IsZero() || at.Sub(lostAt) > bound {
		t.Fatalf("unit stopped at %v, lost at %v (bound %v)", at, lostAt, bound)
	}
	if !pidGone(s.State.PID, time.Second) {
		t.Fatalf("the adopted session (pid %d) still runs", s.State.PID)
	}
	assertNothingSent(t, func(m string) int { return len(f.CallsNamed(m)) })
	if !strings.Contains(logs.String(), "item ship-it/1: claim lost (item is cancelled); session stopped\n") {
		t.Fatalf("log = %q", logs.String())
	}
}

func TestLostRefusal(t *testing.T) {
	for msg, lost := range map[string]bool{
		"item is ready, not running":        true,
		"item is cancelled, not running":    true,
		"item is not held by this node":     true,
		"reportWorkItemProgress: not yours": true,
		"no such item":                      true,
		"budget exhausted":                  false,
	} {
		got := lostRefusal(&gateway.Error{Message: msg}) != ""
		if got != lost {
			t.Errorf("%q: lost = %v, want %v", msg, got, lost)
		}
	}
	if lostRefusal(errors.New("item is ready, not running")) != "" {
		t.Error("a transport error is never a lost claim")
	}
	if lostRefusal(&gateway.Error{Message: "not running", Unsupported: true}) != "" {
		t.Error("an older platform is never a lost claim")
	}
}

func TestLostByRead(t *testing.T) {
	const me = "n-1"
	for _, c := range []struct {
		item *gateway.WorkItem
		want string
	}{
		{&gateway.WorkItem{Status: "running", NodeID: me}, ""},
		{&gateway.WorkItem{Status: "claimed", NodeID: me}, ""},
		{&gateway.WorkItem{Status: "running", NodeID: "n-2"}, "item is running on node n-2"},
		{&gateway.WorkItem{Status: "ready"}, "item is ready"},
		{&gateway.WorkItem{Status: "cancelled", NodeID: me}, "item is cancelled"},
		{&gateway.WorkItem{Status: "merged", NodeID: me}, "item is merged"},
		{nil, "item not on the platform"},
	} {
		if got := lostByRead(c.item, me); got != c.want {
			t.Errorf("%+v: %q, want %q", c.item, got, c.want)
		}
	}
}
