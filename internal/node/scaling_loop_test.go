package node

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// switchMachine is a MachineProbe that can be switched: busy or idle, as the test sets it.
type switchMachine struct{ busy atomic.Bool }

func (m *switchMachine) Read() (MachineReading, error) {
	if m.busy.Load() {
		return MachineReading{Load1: 16, CPUs: 8, MemAvailable: 8 << 30}, nil
	}
	return MachineReading{Load1: 0.5, CPUs: 8, MemAvailable: 8 << 30}, nil
}

// fakeWindows is a WindowSource the test sets.
type fakeWindows struct {
	mu sync.Mutex
	ws Windows
	ok bool
}

func (f *fakeWindows) Source() (Windows, bool) { f.mu.Lock(); defer f.mu.Unlock(); return f.ws, f.ok }
func (f *fakeWindows) Set(ws Windows)          { f.mu.Lock(); f.ws, f.ok = ws, true; f.mu.Unlock() }

// underPace is a week with room: half spent, two hours to its reset —
// nine slots at the default burn, more than any max here.
func underPace() Windows {
	return Windows{SevenDay: &WindowReading{Utilization: 0.5, ResetsAt: t0.Add(2 * time.Hour)}}
}

// overPace is a week already past its 0.95 target: it affords nothing.
func overPace() Windows {
	return Windows{SevenDay: &WindowReading{Utilization: 0.97, ResetsAt: t0.Add(72 * time.Hour)}}
}

// scaled is a node over both orgs with that capacity, a fake clock at
// t0, fake windows and an idle fake machine. Builds block until release
// gets a value; each notes whether its ctx was cancelled.
type scaled struct {
	*priorityFake
	loop      *Loop
	logs      *strings.Builder
	clock     *clock
	windows   *fakeWindows
	machine   *switchMachine
	release   chan struct{}
	cancelled atomic.Int32
}

func newScaled(t *testing.T, c Capacity, blocking bool) *scaled {
	p := newPriorityFake(t)
	p.register()
	loop, logs := p.loop(1)
	loop.Cfg.Capacity = c
	s := &scaled{priorityFake: p, loop: loop, logs: logs, clock: &clock{now: t0}, windows: &fakeWindows{}, machine: &switchMachine{}, release: make(chan struct{}, 16)}
	loop.Now, loop.Windows, loop.Machine = s.clock.Now, s.windows.Source, s.machine
	if blocking {
		loop.Dispatch = func(ctx context.Context, _ gateway.WorkItem) {
			<-s.release
			if ctx.Err() != nil {
				s.cancelled.Add(1)
			}
		}
		t.Cleanup(func() {
			for range 16 {
				s.release <- struct{}{}
			}
			loop.wg.Wait()
		})
	}
	return s
}

// at ticks at t0+d and returns what the tick claimed; blocking builds
// are still running when it returns.
func (s *scaled) at(d time.Duration) []string {
	s.t.Helper()
	s.clock.Set(t0.Add(d))
	before := len(s.CallsNamed("claimWorkItem"))
	s.loop.Tick(context.Background())
	var claimed []string
	for _, c := range s.CallsNamed("claimWorkItem")[before:] {
		claimed = append(claimed, c.Input["aggregateId"].(string))
	}
	return claimed
}

// finishOne lets one build end and waits until the loop counts it gone.
func (s *scaled) finishOne() {
	s.t.Helper()
	want := s.loop.Running() - 1
	s.release <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for s.loop.Running() != want {
		if time.Now().After(deadline) {
			s.t.Fatalf("running stays %d, want %d", s.loop.Running(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// capacityEvery ticks every 30 simulated seconds from `from` to `to` and
// returns the capacity in force after each, by the minute and a half.
func (s *scaled) capacityEvery(from, to time.Duration) map[time.Duration]int {
	out := map[time.Duration]int{}
	for d := from; d <= to; d += 30 * time.Second {
		s.at(d)
		s.loop.wg.Wait()
		out[d] = s.loop.Capacity()
	}
	return out
}

func stepsOf(t *testing.T, got map[time.Duration]int, want func(time.Duration) int) {
	t.Helper()
	for d, n := range got {
		if n != want(d) {
			t.Fatalf("at +%s capacity %d, want %d (all: %v)", d, n, want(d), got)
		}
	}
}

// (1) Under weekly pace a {1, 3} node starts at 1 and steps to 2 at +5m
// and 3 at +10m — one step per 5 minutes, never faster — and holds at
// max; (2) over pace it steps down again to min, one per 5 minutes.
func TestScalingStepsUpToMaxAndDownToMin(t *testing.T) {
	s := newScaled(t, Capacity{Min: 1, Max: 3}, false)
	s.windows.Set(underPace())
	stepsOf(t, s.capacityEvery(0, 14*time.Minute), func(d time.Duration) int {
		return min(1+int(d/(5*time.Minute)), 3)
	})
	if got, ok := s.loop.Scaling(); !ok || got != (Scaling{Capacity: 3, Min: 1, Max: 3, Reason: ReasonMax}) {
		t.Fatalf("scaling = %+v, %v", got, ok)
	}

	// the capacity last moved at +10m: over pace, the next step is at +15m
	s.windows.Set(overPace())
	stepsOf(t, s.capacityEvery(15*time.Minute-30*time.Second, 30*time.Minute), func(d time.Duration) int {
		switch {
		case d < 15*time.Minute:
			return 3
		case d < 20*time.Minute:
			return 2
		}
		return 1
	})
	if got, _ := s.loop.Scaling(); got != (Scaling{Capacity: 1, Min: 1, Max: 3, Reason: ReasonWeeklyPace}) {
		t.Fatalf("scaling = %+v", got)
	}
	for _, line := range []string{"capacity 1–3: scaling by the plan's windows from 1", "capacity 1 → 2: max", "capacity 2 → 3: max",
		"capacity 3 → 2: weekly pace", "capacity 2 → 1: weekly pace"} {
		if strings.Count(s.logs.String(), line) != 1 {
			t.Fatalf("want %q logged once:\n%s", line, s.logs)
		}
	}
}

// (3) A shrink below the running count cancels nothing and claims
// nothing until running falls below the capacity in force.
func TestScalingShrinkOnlyStopsClaiming(t *testing.T) {
	s := newScaled(t, Capacity{Min: 1, Max: 3}, true)
	s.addGoal(orgID, "hot", "high")
	for i, id := range []string{"h1", "h2", "h3", "h4", "h5"} {
		s.addItem(orgID, "hot", id, i+1, false)
	}
	s.windows.Set(underPace())
	var claimed []string
	for d := time.Duration(0); d <= 10*time.Minute; d += time.Minute {
		claimed = append(claimed, s.at(d)...)
	}
	if strings.Join(claimed, ",") != "h1,h2,h3" || s.loop.Running() != 3 {
		t.Fatalf("climbing claimed %v, %d running; want h1,h2,h3", claimed, s.loop.Running())
	}

	s.windows.Set(overPace())
	for d := 11 * time.Minute; d <= 21*time.Minute; d += time.Minute {
		if got := s.at(d); len(got) != 0 {
			t.Fatalf("at +%s, capacity %d under %d running, claimed %v", d, s.loop.Capacity(), s.loop.Running(), got)
		}
	}
	if s.loop.Capacity() != 1 || s.loop.Running() != 3 {
		t.Fatalf("capacity %d, running %d; want 1 and 3", s.loop.Capacity(), s.loop.Running())
	}
	s.finishOne()
	if got := s.at(22 * time.Minute); len(got) != 0 {
		t.Fatalf("2 running at capacity 1, claimed %v", got)
	}
	s.finishOne()
	if got := s.at(23 * time.Minute); len(got) != 0 {
		t.Fatalf("1 running at capacity 1, claimed %v", got)
	}
	s.finishOne()
	if got := s.at(24 * time.Minute); strings.Join(got, ",") != "h4" {
		t.Fatalf("none running at capacity 1, claimed %v, want h4", got)
	}
	if n := s.cancelled.Load(); n != 0 {
		t.Fatalf("%d running build(s) were cancelled by the shrink", n)
	}
}

// (4) At min, with min < max, only urgent and high goals' items are
// claimed; above min, normal and low ones are too.
func TestScalingAtMinClaimsOnlyUrgentAndHigh(t *testing.T) {
	s := newScaled(t, Capacity{Min: 1, Max: 3}, false)
	s.addGoal(orgID, "steady", "normal")
	s.addGoal(orgID, "someday", "low")
	s.addGoal(org2, "hot", "high")
	s.addGoal(org2, "fire", "urgent")
	s.addItem(orgID, "steady", "normal-1", 1, false)
	s.addItem(orgID, "someday", "low-1", 2, false)
	s.addItem(org2, "hot", "high-1", 1, false)
	s.addItem(org2, "fire", "urgent-1", 2, false)
	s.windows.Set(overPace())

	var claimed []string
	for d := time.Duration(0); d <= 4*time.Minute; d += time.Minute {
		claimed = append(claimed, s.at(d)...)
		s.loop.wg.Wait()
	}
	if strings.Join(claimed, ",") != "urgent-1,high-1" {
		t.Fatalf("at min claimed %v, want urgent-1,high-1", claimed)
	}
	if s.Item("normal-1")["status"] != "ready" || s.Item("low-1")["status"] != "ready" {
		t.Fatalf("normal/low claimed at min: %v, %v", s.Item("normal-1")["status"], s.Item("low-1")["status"])
	}

	s.windows.Set(underPace())
	if got := s.at(5 * time.Minute); strings.Join(got, ",") != "normal-1,low-1" || s.loop.Capacity() != 2 {
		t.Fatalf("at capacity %d claimed %v, want normal-1,low-1 at 2", s.loop.Capacity(), got)
	}
}

// (5) A plain `capacity: 2` runs as before: two items of any priority,
// whatever the windows and the machine say, and the capacity never
// moves; no scaling to report.
func TestScalingFixedCapacityIsUnchanged(t *testing.T) {
	s := newScaled(t, Fixed(2), true)
	s.addGoal(orgID, "someday", "low")
	s.addGoal(orgID, "steady", "normal")
	s.addItem(orgID, "someday", "low-1", 1, false)
	s.addItem(orgID, "steady", "normal-1", 2, false)
	s.addItem(orgID, "steady", "normal-2", 3, false)
	s.windows.Set(overPace())
	s.machine.busy.Store(true)

	var claimed []string
	for d := time.Duration(0); d <= 20*time.Minute; d += time.Minute {
		claimed = append(claimed, s.at(d)...)
		if s.loop.Capacity() != 2 {
			t.Fatalf("at +%s capacity %d", d, s.loop.Capacity())
		}
	}
	if strings.Join(claimed, ",") != "normal-1,normal-2" {
		t.Fatalf("claimed %v, want normal-1,normal-2", claimed)
	}
	if _, ok := s.loop.Scaling(); ok {
		t.Fatal("a fixed node reports scaling")
	}
	if strings.Contains(s.logs.String(), "capacity") {
		t.Fatalf("a fixed node logged a capacity change:\n%s", s.logs)
	}
}

// (6) Reload to a narrower range clamps the capacity in force at once,
// without waiting for a step; a range above it lifts it at once.
func TestScalingReloadClampsIntoTheNewRange(t *testing.T) {
	p := newPriorityFake(t)
	loop, _, logs := p.liveLoop(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: {min: 3, max: 5}\n")
	c, w := &clock{now: t0}, &fakeWindows{}
	w.Set(underPace())
	loop.Now, loop.Windows, loop.Machine = c.Now, w.Source, &switchMachine{}

	loop.Tick(context.Background())
	if got, _ := loop.Scaling(); got.Capacity != 3 {
		t.Fatalf("a new {3, 5} node starts at %d, want 3", got.Capacity)
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: {min: 1, max: 2}\n")
	c.Set(t0.Add(30 * time.Second))
	loop.Tick(context.Background())
	if got, ok := loop.Scaling(); !ok || got != (Scaling{Capacity: 2, Min: 1, Max: 2, Reason: ReasonMax}) {
		t.Fatalf("after reload to {1, 2}: %+v", got)
	}
	if !strings.Contains(logs.String(), "capacity 3 → 2: clamped into 1–2") {
		t.Fatalf("the clamp is not logged:\n%s", logs)
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: {min: 4, max: 6}\n")
	c.Set(t0.Add(time.Minute))
	loop.Tick(context.Background())
	if loop.Capacity() != 4 {
		t.Fatalf("after reload to {4, 6}: capacity %d, want 4", loop.Capacity())
	}
	writeNodeYAML(t, "slug: box\nname: Box\norgs: [acme]\ncapacity: 2\n")
	loop.Tick(context.Background())
	if _, ok := loop.Scaling(); ok || loop.Capacity() != 2 {
		t.Fatalf("after reload to a plain 2: capacity %d", loop.Capacity())
	}
}

// A busy machine holds a ranged node at what it runs: no growth, with
// the reason `machine load`.
func TestScalingBusyMachineHoldsAtRunning(t *testing.T) {
	s := newScaled(t, Capacity{Min: 1, Max: 3}, true)
	s.addGoal(orgID, "hot", "high")
	s.addItem(orgID, "hot", "h1", 1, false)
	s.addItem(orgID, "hot", "h2", 2, false)
	s.windows.Set(underPace())
	s.machine.busy.Store(true)
	var claimed []string
	for d := time.Duration(0); d <= 15*time.Minute; d += time.Minute {
		claimed = append(claimed, s.at(d)...)
	}
	if got, _ := s.loop.Scaling(); got.Capacity != 1 || got.Reason != ReasonMachineLoad || strings.Join(claimed, ",") != "h1" {
		t.Fatalf("busy: scaling %+v, claimed %v; want 1 for machine load, h1", got, claimed)
	}
}

// Without a WindowSource the loop reads the limits store: the highest
// utilization across accounts, per window.
func TestWindowsOfTakesTheHighestAcrossAccounts(t *testing.T) {
	a, b := reading(0.2, 0.7), reading(0.6, 0.3)
	b.SevenDay.ResetsAt = weekOver.Add(time.Hour)
	l := &Loop{Limits: &Limits{}}
	if _, ok := l.windows(); ok {
		t.Fatal("no reading reads as windows")
	}
	l.Limits.Observe("one", a)
	l.Limits.Observe("two", b)
	ws, ok := l.windows()
	if !ok || ws.FiveHour.Utilization != 0.6 || ws.SevenDay.Utilization != 0.7 || !ws.SevenDay.ResetsAt.Equal(weekOver) {
		t.Fatalf("windows = %+v %+v, %v", ws.FiveHour, ws.SevenDay, ok)
	}
}

// A ranged node heartbeats scaling — the capacity in force, the range
// and the reason — and tells Scaled so; a plain `capacity: N` heartbeats
// N as its min and max, reason `fixed` (ADR-018), and tells Scaled it is
// fixed.
func TestScalingHeartbeat(t *testing.T) {
	lastScaling := func(s *scaled) (any, bool) {
		hb := s.CallsNamed("heartbeatNode")
		if len(hb) == 0 {
			t.Fatal("no heartbeat")
		}
		v, ok := hb[len(hb)-1].Input["scaling"]
		return v, ok
	}

	ranged := newScaled(t, Capacity{Min: 1, Max: 4}, false)
	ranged.windows.Set(underPace())
	var told []Scaling
	ranged.loop.Scaled = func(s Scaling, ok bool) {
		if !ok {
			t.Fatal("a ranged node told Scaled it is fixed")
		}
		told = append(told, s)
	}
	ranged.at(0)
	ranged.at(5 * time.Minute)
	v, ok := lastScaling(ranged)
	s, _ := v.(map[string]any)
	if !ok || s["capacity"] != 2.0 || s["min"] != 1.0 || s["max"] != 4.0 || s["reason"] != "max" {
		t.Fatalf("ranged heartbeat scaling = %#v, %v", v, ok)
	}
	if len(told) != 2 || told[1] != (Scaling{Capacity: 2, Min: 1, Max: 4, Reason: ReasonMax}) {
		t.Fatalf("Scaled was told %+v", told)
	}
	if got := told[1].String(); got != "2/1–4 (max)" {
		t.Fatalf("status shows %q", got)
	}

	fixed := newScaled(t, Fixed(3), false)
	fixed.windows.Set(underPace())
	fixedTold := 0
	fixed.loop.Scaled = func(_ Scaling, ok bool) {
		if ok {
			t.Fatal("a fixed node told Scaled it is ranged")
		}
		fixedTold++
	}
	fixed.at(0)
	fixed.at(5 * time.Minute)
	v, ok = lastScaling(fixed)
	if s, _ := v.(map[string]any); !ok || s["capacity"] != 3.0 || s["min"] != 3.0 || s["max"] != 3.0 || s["reason"] != "fixed" {
		t.Fatalf("fixed heartbeat scaling = %#v, %v; want its capacity 3 as 3–3, fixed", v, ok)
	}
	if fixedTold != 2 {
		t.Fatalf("Scaled told %d times, want 2", fixedTold)
	}
}
