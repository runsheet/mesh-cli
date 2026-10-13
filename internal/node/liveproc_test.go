package node

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// The live probe (goal a-stalled-session-is-stopped-not-orphaned): the
// free slots are capacity less the build sessions alive on the host,
// orphans included, and an orphan no live item of this node's claims is
// stopped by the periodic sweep.

// liveFake is a priorityFake with three ready items in acme and a user
// manager running a build scope for each of builds; each build's item
// is added running on this node unless the test changes it.
func liveFake(t *testing.T, builds ...string) (*priorityFake, *sweepSystemctl) {
	t.Helper()
	p := newPriorityFake(t)
	p.addGoal(orgID, "run", "normal")
	for i := 1; i <= 3; i++ {
		p.addItem(orgID, "run", fmt.Sprintf("ready-%d", i), i, false)
	}
	sc := &sweepSystemctl{procs: map[string][]sessionunit.Process{}}
	for i, b := range builds {
		p.addItemRow(orgID, "run", b, 10+i, map[string]any{"status": "running", "nodeId": p.scopes[0].NodeID})
		unit := sessionunit.UnitName("build", b)
		sc.scopes = append(sc.scopes, sessionunit.Scope{Unit: unit, Active: "active"})
		sc.procs[unit] = []sessionunit.Process{{PID: 5000 + i, Comm: "claude"}}
	}
	return p, sc
}

// unitOf is the scope sc runs for item.
func unitOf(sc *sweepSystemctl, item string) string {
	for u := range sc.procs {
		if _, subject, _ := sessionunit.ParseUnitName(u); subject == item {
			return u
		}
	}
	return ""
}

// liveLoop is p's loop at that capacity with a live probe on sc that
// sweeps every `every` ticks and finds no state files.
func liveLoop(p *priorityFake, sc *sweepSystemctl, capacity, every int) (*Loop, *strings.Builder) {
	loop, logs := p.loop(capacity)
	loop.Live = &LiveProbe{Systemctl: sc, SweepEvery: every,
		Find: func(string, []string) ([]FoundSession, error) { return nil, nil }}
	return loop, logs
}

// liveTick runs one tick and returns how many items it claimed.
func liveTick(p *priorityFake, loop *Loop) int {
	before := len(p.CallsNamed("claimWorkItem"))
	loop.Tick(context.Background())
	loop.wg.Wait()
	return len(p.CallsNamed("claimWorkItem")) - before
}

// (1) Two live build scopes the loop does not track fill a capacity of
// two: nothing is claimed, and the capacity line says why.
func TestLiveUntrackedScopesFillCapacity(t *testing.T) {
	p, sc := liveFake(t, "orphan-a", "orphan-b")
	loop, logs := liveLoop(p, sc, 2, 5)
	if n := liveTick(p, loop); n != 0 {
		t.Fatalf("claimed %d with 2 live sessions at capacity 2\n%s", n, logs)
	}
	want := "capacity: 2 live agent sessions (2 orphans) against capacity 2; claiming nothing"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log lacks %q:\n%s", want, logs)
	}
	if sc.stopped() {
		t.Fatalf("a live item of this node's was stopped: %q", sc.calls)
	}
}

// (2) Capacity 8 with seven tracked builds and one orphan: no ninth
// claim. With the orphan's scope gone the eighth slot is claimed.
func TestLiveOrphanTakesTheLastSlot(t *testing.T) {
	builds := []string{"tracked-1", "tracked-2", "tracked-3", "tracked-4", "tracked-5", "tracked-6", "tracked-7", "orphan-1"}
	p, sc := liveFake(t, builds...)
	loop, logs := liveLoop(p, sc, 8, 5)
	loop.running = map[string]bool{}
	for _, b := range builds[:7] {
		loop.running[b] = true
	}
	if n := liveTick(p, loop); n != 0 {
		t.Fatalf("claimed %d with 7 tracked and 1 orphan at capacity 8\n%s", n, logs)
	}
	if live, orphans := loop.LiveSessions(); live != 8 || orphans != 1 {
		t.Fatalf("live %d, orphans %d; want 8, 1", live, orphans)
	}
	if !strings.Contains(logs.String(), "capacity: 8 live agent sessions (1 orphans) against capacity 8; claiming nothing") {
		t.Fatalf("no capacity line:\n%s", logs)
	}
	sc.scopes = slices.DeleteFunc(sc.scopes, func(s sessionunit.Scope) bool { return strings.Contains(s.Unit, "orphan-1") })
	if n := liveTick(p, loop); n != 1 {
		t.Fatalf("claimed %d with 7 live at capacity 8; want 1\n%s", n, logs)
	}
}

// (3) An orphan whose item has gone terminal is stopped by the next
// periodic sweep, and the slot it held is free on that tick.
func TestLiveSweepStopsTerminalOrphan(t *testing.T) {
	p, sc := liveFake(t, "orphan-a", "orphan-b")
	loop, logs := liveLoop(p, sc, 2, 2)
	unit, other := unitOf(sc, "orphan-a"), unitOf(sc, "orphan-b")
	liveTick(p, loop) // tick 1 sweeps: both live, ours
	p.WorkItems["orphan-a"]["status"] = "merged"
	liveTick(p, loop) // tick 2: no sweep, still counted
	if live, _ := loop.LiveSessions(); live != 2 {
		t.Fatalf("live %d before the sweep; want 2", live)
	}
	if sc.stopped() {
		t.Fatalf("stopped between sweeps: %q", sc.calls)
	}
	n := liveTick(p, loop) // tick 3 sweeps
	if !slices.Contains(sc.calls, "stop "+unit+".scope") {
		t.Fatalf("orphan not stopped: %q\n%s", sc.calls, logs)
	}
	want := fmt.Sprintf("capacity: orphan: stopped %s (item orphan-a, merged on the platform): killed claude[5000]", unit)
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log lacks %q:\n%s", want, logs)
	}
	if live, orphans := loop.LiveSessions(); live != 1 || orphans != 1 {
		t.Fatalf("live %d, orphans %d after the sweep; want 1, 1", live, orphans)
	}
	if n != 1 {
		t.Fatalf("claimed %d once the slot freed; want 1\n%s", n, logs)
	}
	if slices.Contains(sc.calls, "stop "+other+".scope") {
		t.Fatalf("the live orphan was stopped: %q", sc.calls)
	}
}

// (4) An orphan whose item is still live and held by this node is
// counted, never stopped, sweep after sweep.
func TestLiveOrphanOfALiveItemIsCountedNotStopped(t *testing.T) {
	p, sc := liveFake(t, "orphan-a")
	loop, logs := liveLoop(p, sc, 2, 1)
	for range 3 {
		liveTick(p, loop)
		if live, orphans := loop.LiveSessions(); live != 1 || orphans != 1 {
			t.Fatalf("live %d, orphans %d; want 1, 1", live, orphans)
		}
	}
	if sc.stopped() {
		t.Fatalf("a live item's orphan was stopped: %q\n%s", sc.calls, logs)
	}
}

// Another node's session on the same host — its item held elsewhere and
// no state file of ours naming it — is neither stopped nor counted.
func TestLiveForeignScopeLeftAlone(t *testing.T) {
	p, sc := liveFake(t, "theirs")
	p.WorkItems["theirs"]["nodeId"] = "another-node"
	loop, _ := liveLoop(p, sc, 1, 1)
	if n := liveTick(p, loop); n != 1 {
		t.Fatalf("claimed %d; another node's session took this node's slot", n)
	}
	if sc.stopped() {
		t.Fatalf("another node's session was stopped: %q", sc.calls)
	}
}

// With no systemd the found sessions whose pid is alive are counted; a
// dead one is not.
func TestLiveFoundSessionsWithoutSystemd(t *testing.T) {
	p, _ := liveFake(t)
	p.addItemRow(orgID, "run", "found-1", 20, map[string]any{"status": "running", "nodeId": p.scopes[0].NodeID})
	loop, logs := p.loop(1)
	loop.Live = &LiveProbe{SweepEvery: 1, Find: func(string, []string) ([]FoundSession, error) {
		return []FoundSession{
			{Dir: "/w/found-1.session", State: sessionunit.State{Kind: "build", ItemID: "found-1", PID: 1}, Unit: aliveUnit{true}},
			{Dir: "/w/dead.session", State: sessionunit.State{Kind: "build", ItemID: "dead", PID: 2}, Unit: aliveUnit{false}},
		}, nil
	}}
	if n := liveTick(p, loop); n != 0 {
		t.Fatalf("claimed %d with a live found session at capacity 1\n%s", n, logs)
	}
	if live, orphans := loop.LiveSessions(); live != 1 || orphans != 1 {
		t.Fatalf("live %d, orphans %d; want 1, 1", live, orphans)
	}
}

// aliveUnit is a unit whose Alive answers alive and whose Kill kills
// nothing.
type aliveUnit struct{ alive bool }

func (u aliveUnit) Name() string { return "fake" }
func (u aliveUnit) Start(sessionunit.Spec) (sessionunit.Handle, error) {
	return sessionunit.Handle{}, nil
}
func (u aliveUnit) Alive(sessionunit.Handle) bool                          { return u.alive }
func (u aliveUnit) Stop(sessionunit.Handle) error                          { return nil }
func (u aliveUnit) Kill(sessionunit.Handle) ([]sessionunit.Process, error) { return nil, nil }
