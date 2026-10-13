//go:build !windows

package node

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway/gatewaytest"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sessionunit"
)

// A build starved on an overloaded host is stopped and released with the
// overload reason, charging no attempt (goal
// a-stalled-session-is-stopped-not-orphaned, item 4; ADR-029).

// overloadedHost is dl360 on 2026-10-13: load 138.2 on 96 cores.
var overloadedHost = MachineReading{Load1: 138.2, CPUs: 96, MemAvailable: 64 << 30}

// quietHost is the same machine at rest.
var quietHost = MachineReading{Load1: 10, CPUs: 96, MemAvailable: 64 << 30}

const overloadReason = "host overloaded: load/core 1.44 (138.2 on 96 cores)"

// starveUnit is stopUnit whose Kill goes through its own Stop, as
// SystemdScope's Kill ends in `systemctl --user stop`: both are recorded.
type starveUnit struct{ stopUnit }

func (u *starveUnit) Kill(h sessionunit.Handle) ([]sessionunit.Process, error) {
	u.mu.Lock()
	u.kills = append(u.kills, time.Now())
	u.mu.Unlock()
	return nil, u.stopUnit.Stop(h)
}

func (u *starveUnit) stopCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.stops)
}

// silentBuild is a loop of capacity 1 running item 1 on a fake claude
// that says one thing and then nothing for a minute, with node.yaml's
// host.starve_after at 400ms and the machine probe reading m.
func silentBuild(t *testing.T, m MachineReading) (*Loop, *gatewaytest.Fake, *starveUnit, *syncBuf, FoundSession) {
	t.Helper()
	fastFollow(t)
	t.Setenv("SLOW_EVENTS", "1")
	t.Setenv("SLOW_SLEEP", "60")
	f := seed(t)
	cfg := registeredNode(t, f)
	cfg.Host = HostConfig{StarveAfter: 400 * time.Millisecond}
	root := t.TempDir()
	u := &starveUnit{}
	claude := &harness.ClaudeCode{Binary: slowClaudeScript(t), Unit: u}
	loop, runner := adoptNode(t, f, cfg, root, claude)
	runner.Machine = fakeMachine{m: m}
	logs := &syncBuf{}
	runner.Log = log.New(logs, "", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	loop.Tick(ctx)
	s := waitLiveSession(t, root, cfg.NodeID)
	if n := loop.Running(); n != 1 {
		t.Fatalf("Running() = %d, want 1", n)
	}
	t.Cleanup(func() {
		f.SetItem(item1, map[string]any{"status": "cancelled"})
		loop.wg.Wait()
	})
	return loop, f, u, logs, s
}

// (1) A build silent past starve_after while the host is overloaded is
// stopped — its unit stopped and killed, its process gone, Running()
// down by one — and released once with `stalled: host overloaded:
// load/core …`, never failed or finished.
func TestStarvedBuildOnOverloadedHostIsReleased(t *testing.T) {
	loop, f, u, logs, s := silentBuild(t, overloadedHost)
	calls := func(m string) int { return len(f.CallsNamed(m)) }
	deadline := time.Now().Add(400*time.Millisecond + lostBound(50*time.Millisecond) + 2*time.Second)
	for loop.Running() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Running() = %d; the starved build was never stopped", loop.Running())
		}
		time.Sleep(10 * time.Millisecond)
	}
	loop.wg.Wait()
	if u.stopCount() == 0 || u.killCount() == 0 {
		t.Fatalf("unit stops %d, kills %d; want both recorded", u.stopCount(), u.killCount())
	}
	if !pidGone(s.State.PID, time.Second) {
		t.Fatalf("the starved session (pid %d) still runs", s.State.PID)
	}
	if n := calls("releaseWorkItem"); n != 1 {
		t.Fatalf("releaseWorkItem sent %d time(s), want 1", n)
	}
	for _, m := range []string{"failWorkItem", "finishWorkItem"} {
		if n := calls(m); n != 0 {
			t.Fatalf("%s sent %d time(s) for a starved build", m, n)
		}
	}
	if st := f.Item(item1)["status"]; st != "ready" {
		t.Fatalf("item is %v after the release, want ready", st)
	}
	want := "item ship-it/1: stalled on an overloaded host; released: stalled: " + overloadReason
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log = %q, want %q", logs.String(), want)
	}
}

// The release's note is the overload: `stalled: host overloaded:
// load/core …`.
func TestStarvedReleaseNote(t *testing.T) {
	_, f, _, _, _ := silentBuild(t, overloadedHost)
	waitFor(t, func() bool { return len(f.CallsNamed("releaseWorkItem")) > 0 })
	rel := f.CallsNamed("releaseWorkItem")
	if len(rel) != 1 {
		t.Fatalf("releases %d, want 1", len(rel))
	}
	note, _ := rel[0].Input["note"].(string)
	if !strings.HasPrefix(note, "stalled: host overloaded: load/core") || note != StallPrefix+overloadReason {
		t.Fatalf("release note %q, want %q", note, StallPrefix+overloadReason)
	}
}

// (2) The same silence on a host that is not overloaded is left to run:
// no stop, no release.
func TestSilentBuildOnQuietHostRuns(t *testing.T) {
	loop, f, u, logs, s := silentBuild(t, quietHost)
	calls := func(m string) int { return len(f.CallsNamed(m)) }
	time.Sleep(400*time.Millisecond + 1500*time.Millisecond)
	if n := loop.Running(); n != 1 {
		t.Fatalf("Running() = %d, want the silent build still running", n)
	}
	if u.killCount() != 0 || u.stopCount() != 0 {
		t.Fatalf("a silent build on a quiet host was stopped: stops %d, kills %d", u.stopCount(), u.killCount())
	}
	if pidGone(s.State.PID, 50*time.Millisecond) {
		t.Fatal("the session's process is gone")
	}
	if n := calls("releaseWorkItem"); n != 0 {
		t.Fatalf("releaseWorkItem sent %d time(s) on a quiet host", n)
	}
	if strings.Contains(logs.String(), "stalled on an overloaded host") {
		t.Fatalf("log = %q", logs.String())
	}
}

// (3) A stall the platform recorded while the host was overloaded is
// found by item 1's lost-claim check, and its log line carries the
// overload reason; nothing is sent, as for any lost claim.
func TestPlatformStallOnOverloadedHostNamesTheOverload(t *testing.T) {
	fastFollow(t)
	t.Setenv("SLOW_EVENTS", "1000")
	t.Setenv("SLOW_SLEEP", "0.05")
	f := seed(t)
	u := &stopUnit{}
	h := &harness.ClaudeCode{Binary: slowClaudeScript(t), Unit: u}
	r, ws, item, _ := runnerOnFake(t, f, h, nil)
	r.ProgressEvery = 100 * time.Millisecond
	r.Machine = fakeMachine{m: overloadedHost}
	logs := &syncBuf{}
	r.Log = log.New(logs, "", 0)
	done := make(chan error, 1)
	go func() { done <- r.RunItem(context.Background(), item) }()
	waitLiveSession(t, ws.root, r.Cfg.NodeID)
	f.SetItem(item1, lostCases[0].fields) // the platform's stall
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunItem = %v", err)
		}
	case <-time.After(lostBound(r.ProgressEvery) + 2*time.Second):
		t.Fatal("RunItem still running after the platform stalled the item")
	}
	if u.killCount() == 0 {
		t.Fatal("the session's unit was never killed")
	}
	assertNothingSent(t, func(m string) int { return len(f.CallsNamed(m)) })
	want := "item ship-it/1: claim lost (item is ready; " + overloadReason + "); session stopped"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log = %q, want %q", logs.String(), want)
	}
}

// With no machine probe, or starve_after off, nothing is judged starved.
func TestCheckStarvedNeedsAnOverloadedReading(t *testing.T) {
	r := &Runner{Cfg: &Config{Host: HostConfig{StarveAfter: time.Millisecond}}}
	b := &buildRun{r: r}
	_, b.watch = newClaimWatch(context.Background())
	b.lastHeard.Store(time.Now().Add(-time.Hour).UnixNano())
	if !b.checkStarved() || b.watch.starved() != "" {
		t.Fatal("no machine probe starved the build")
	}
	r.Machine = fakeMachine{m: quietHost}
	if !b.checkStarved() || b.watch.starved() != "" {
		t.Fatal("a quiet host starved the build")
	}
	r.Machine = fakeMachine{m: overloadedHost}
	if b.checkStarved() || b.watch.starved() != overloadReason {
		t.Fatalf("an overloaded host did not starve the build: %q", b.watch.starved())
	}
	b2 := &buildRun{r: r}
	_, b2.watch = newClaimWatch(context.Background())
	b2.heard()
	if !b2.checkStarved() {
		t.Fatal("a build heard from just now was starved")
	}
}
