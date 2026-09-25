package node

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)

// world is a simulated plan: each window rises by its per-slot-minute
// burn times the slots running, and resets at its time.
type world struct {
	now        time.Time
	u5, u7     float64
	reset5     time.Time
	reset7     time.Time
	per5, per7 float64 // utilization per slot-minute
	no5, no7   bool    // the window is not reported
	load       MachineLoad
	scaler     Scaler
	current    int
	min, max   int
	reasons    []string
	capacities []int
}

func (w *world) obs() Observation {
	o := Observation{At: w.now, Running: w.current, Load: w.load}
	if !w.no5 {
		o.FiveHour = &Window{Utilization: w.u5, ResetsAt: w.reset5}
	}
	if !w.no7 {
		o.SevenDay = &Window{Utilization: w.u7, ResetsAt: w.reset7}
	}
	return o
}

// run advances the clock a minute at a time for the given minutes: the
// windows rise with what ran, the scaler observes and decides, and the
// capacity it gives is what runs next. No decision moves more than one.
func (w *world) run(t *testing.T, minutes int) {
	t.Helper()
	for range minutes {
		w.now = w.now.Add(time.Minute)
		w.u5 += w.per5 * float64(w.current)
		w.u7 += w.per7 * float64(w.current)
		w.scaler.Observe(w.obs())
		next, reason := w.scaler.Decide(w.now, w.current, w.min, w.max)
		if d := next - w.current; d > 1 || d < -1 {
			t.Fatalf("at %s: %d → %d, more than one step", w.now.Sub(t0), w.current, next)
		}
		w.current = next
		w.reasons = append(w.reasons, reason)
		w.capacities = append(w.capacities, next)
	}
}

func (w *world) reason() string { return w.reasons[len(w.reasons)-1] }

// underPace is a world with low burn and far resets: the week and the
// five hours both afford more than any max below here.
func underPace(current, min, max int) *world {
	return &world{now: t0, current: current, min: min, max: max,
		u5: 0.10, reset5: t0.Add(4 * time.Hour), per5: 0.0002,
		u7: 0.20, reset7: t0.Add(5 * 24 * time.Hour), per7: 0.00002}
}

// assertSteps checks the capacities only ever change a step at a time,
// and at least stepEvery apart.
func assertSteps(t *testing.T, w *world) {
	t.Helper()
	last, lastAt := w.capacities[0], -1000
	for i, c := range w.capacities[1:] {
		if c == last {
			continue
		}
		if i+1-lastAt < int(stepEvery/time.Minute) {
			t.Fatalf("steps at minutes %d and %d: under %s apart (%v)", lastAt, i+1, stepEvery, w.capacities)
		}
		last, lastAt = c, i+1
	}
}

func TestUnderWeeklyPaceStepsUpToMax(t *testing.T) {
	w := underPace(1, 1, 4)
	w.run(t, 60)
	assertSteps(t, w)
	if w.current != 4 || w.reason() != ReasonMax {
		t.Fatalf("ended at %d (%s), want 4 (max); capacities %v", w.current, w.reason(), w.capacities)
	}
	// it climbed through every step
	seen := map[int]bool{}
	for _, c := range w.capacities {
		seen[c] = true
	}
	for c := 1; c <= 4; c++ {
		if !seen[c] {
			t.Fatalf("never ran at %d: %v", c, w.capacities)
		}
	}
}

func TestOverPaceStepsDownToMin(t *testing.T) {
	// the week resets in a day and is half spent, and a slot-minute
	// spends a lot of it: one slot is all it affords
	w := underPace(4, 1, 4)
	w.u7, w.reset7, w.per7 = 0.50, t0.Add(24*time.Hour), 0.0002
	w.run(t, 60)
	assertSteps(t, w)
	if w.current != 1 || w.reason() != ReasonWeekly {
		t.Fatalf("ended at %d (%s), want 1 (weekly pace); capacities %v", w.current, w.reason(), w.capacities)
	}
	for i := 1; i < len(w.capacities); i++ {
		if w.capacities[i] > w.capacities[i-1] {
			t.Fatalf("over pace it stepped up: %v", w.capacities)
		}
	}
}

func TestFiveHourNearTargetCaps(t *testing.T) {
	// the week is loose, but the five hours are at 0.85 of their 0.90
	// with an hour and a half to go
	w := underPace(1, 1, 8)
	w.u5, w.reset5, w.per5 = 0.85, t0.Add(90*time.Minute), 0.00015
	w.run(t, 60)
	assertSteps(t, w)
	if w.current >= w.max || w.reason() != ReasonFiveHour {
		t.Fatalf("ended at %d (%s), want under 8 (five-hour pace); capacities %v", w.current, w.reason(), w.capacities)
	}
	if w.u5 > fiveHourTarget {
		t.Fatalf("the five hours ran to %.3f, past their target", w.u5)
	}
}

func TestHighLoadCaps(t *testing.T) {
	for _, tc := range []struct {
		name          string
		load          MachineLoad
		current, want int
	}{
		{"load at the CPU count", MachineLoad{Load1: 8, CPUs: 8, FreeBytes: 16 << 30}, 3, 2},
		{"load over the CPU count", MachineLoad{Load1: 12.5, CPUs: 8, FreeBytes: 16 << 30}, 3, 2},
		{"memory under 2 GiB", MachineLoad{Load1: 1, CPUs: 8, FreeBytes: 1 << 30}, 3, 2},
		{"at min it stays", MachineLoad{Load1: 9, CPUs: 8, FreeBytes: 16 << 30}, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := underPace(tc.current, 1, 4)
			w.run(t, 30) // enough data for the windows to afford max
			w.scaler.lastStep = time.Time{}
			w.load = tc.load
			w.scaler.Observe(w.obs())
			next, reason := w.scaler.Decide(w.now, tc.current, 1, 4)
			if next != tc.want || reason != ReasonLoad {
				t.Fatalf("Decide = %d (%s), want %d (machine load)", next, reason, tc.want)
			}
		})
	}
	// and under load it never steps up, however much the windows afford
	w := underPace(1, 1, 4)
	w.load = MachineLoad{Load1: 4, CPUs: 4, FreeBytes: 16 << 30}
	w.run(t, 60)
	for _, c := range w.capacities {
		if c != 1 {
			t.Fatalf("stepped up under load: %v", w.capacities)
		}
	}
	if w.reason() != ReasonLoad {
		t.Fatalf("reason %q, want machine load", w.reason())
	}
	// a load under the CPU count and memory over 2 GiB do not cap it
	w = underPace(1, 1, 4)
	w.load = MachineLoad{Load1: 3.9, CPUs: 4, FreeBytes: 3 << 30}
	w.run(t, 60)
	if w.current != 4 || w.reason() != ReasonMax {
		t.Fatalf("with room on the machine ended at %d (%s)", w.current, w.reason())
	}
}

func TestOneStepPerFiveMinutes(t *testing.T) {
	w := underPace(2, 1, 8)
	w.per7 = 0.00001
	// a steady half hour at two slots: the windows afford max
	for range 30 {
		w.now = w.now.Add(time.Minute)
		w.u5 += w.per5 * 2
		w.u7 += w.per7 * 2
		w.scaler.Observe(w.obs())
	}
	at := w.now
	next, reason := w.scaler.Decide(at, 2, 1, 8)
	if next != 3 || reason != ReasonMax {
		t.Fatalf("first decision = %d (%s), want 3 (max)", next, reason)
	}
	if next, _ = w.scaler.Decide(at.Add(4*time.Minute), 3, 1, 8); next != 3 {
		t.Fatalf("4 minutes on, moved to %d; want it to stay at 3", next)
	}
	if next, _ = w.scaler.Decide(at.Add(5*time.Minute), 3, 1, 8); next != 4 {
		t.Fatalf("5 minutes on, %d; want 4", next)
	}
	if next, _ = w.scaler.Decide(at.Add(9*time.Minute), 4, 1, 8); next != 4 {
		t.Fatalf("9 minutes on, moved to %d; want it to stay at 4", next)
	}
}

// history feeds the scaler a sample a minute at the given running, the
// five-hour utilization given per sample.
func history(s *Scaler, from time.Time, running int, u5 ...float64) time.Time {
	at := from
	for i, u := range u5 {
		at = from.Add(time.Duration(i) * time.Minute)
		s.Observe(Observation{At: at, Running: running, FiveHour: &Window{Utilization: u, ResetsAt: from.Add(5 * time.Hour)}})
	}
	return at
}

func TestResetDropsOldSamples(t *testing.T) {
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

	// fifteen minutes climbing hard, then the window resets: what is
	// left after the reset is too little to measure, so the burn is the
	// default and not the climb before it
	var s Scaler
	var us []float64
	for i := range 15 {
		us = append(us, 0.30+0.01*float64(i))
	}
	us = append(us, 0.02, 0.021, 0.022)
	now := history(&s, t0, 2, us...)
	if got := s.burn(now, fiveHour, burnDefault, burnFloor); !near(got, burnDefault) {
		t.Fatalf("burn after a reset = %g, want the default %g", got, burnDefault)
	}

	// ten minutes on after the reset there is enough: the burn is the
	// rise since the reset only
	for i := 3; i <= 12; i++ {
		us = append(us, 0.02+0.001*float64(i))
	}
	var s2 Scaler
	now = history(&s2, t0, 2, us...)
	// 12 minutes after the reset at 2 slots: 24 slot-minutes, a rise of 0.012
	if got, want := s2.burn(now, fiveHour, burnDefault, burnFloor), 0.012/24; !near(got, want) {
		t.Fatalf("burn = %g, want %g (the rise since the reset)", got, want)
	}

	// a gap in the window is a break too
	var s3 Scaler
	now = history(&s3, t0, 2, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9)
	s3.Observe(Observation{At: now.Add(time.Minute), Running: 2})
	now = history(&s3, now.Add(2*time.Minute), 2, 0.5, 0.5001)
	if got := s3.burn(now, fiveHour, burnDefault, burnFloor); !near(got, burnDefault) {
		t.Fatalf("burn across a gap = %g, want the default", got)
	}

	// and samples older than half an hour are dropped by Observe
	var s4 Scaler
	history(&s4, t0, 1, make([]float64, 45)...)
	if first := s4.history[0].At; first.Before(t0.Add(44*time.Minute - historySpan)) {
		t.Fatalf("kept a sample from %s", first.Sub(t0))
	}
}

func TestDefaultBurnBeforeData(t *testing.T) {
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-12 }
	for _, tc := range []struct {
		name    string
		running int
		u5      []float64
		want    float64
	}{
		{"no samples", 1, nil, burnDefault},
		{"one sample", 1, []float64{0.1}, burnDefault},
		{"under ten slot-minutes", 1, []float64{0.10, 0.11, 0.12, 0.13, 0.14, 0.15}, burnDefault},
		{"no rise", 4, []float64{0.2, 0.2, 0.2, 0.2, 0.2, 0.2}, burnDefault},
		{"a tiny rise is floored", 4, []float64{0.2, 0.2, 0.2, 0.2, 0.2, 0.2000001}, burnFloor},
		{"enough data", 4, []float64{0.2, 0.21, 0.22, 0.23, 0.24, 0.25}, 0.05 / 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s Scaler
			now := t0
			if len(tc.u5) > 0 {
				now = history(&s, t0, tc.running, tc.u5...)
			}
			if got := s.burn(now, fiveHour, burnDefault, burnFloor); !near(got, tc.want) {
				t.Fatalf("burn = %g, want %g", got, tc.want)
			}
		})
	}
	// before data the scaler starts cautious: the default burn against a
	// five-hour window with room for about one slot holds it at min
	var s Scaler
	s.Observe(Observation{At: t0, FiveHour: &Window{Utilization: 0.10, ResetsAt: t0.Add(4 * time.Hour)}})
	if next, reason := s.Decide(t0, 1, 1, 4); next != 1 || reason != ReasonFiveHour {
		t.Fatalf("before data Decide = %d (%s), want 1 (five-hour pace)", next, reason)
	}
	// and with no windows at all it is unconstrained
	var free Scaler
	free.Observe(Observation{At: t0, Running: 1})
	if next, reason := free.Decide(t0, 1, 1, 4); next != 2 || reason != ReasonMax {
		t.Fatalf("with no windows Decide = %d (%s), want 2 (max)", next, reason)
	}
}
