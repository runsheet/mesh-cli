package node

import (
	"math"
	"strings"
	"testing"
	"time"
)

// The replay of the dl360's 2026-10-13 (ADR-031, Context): 96 cores, the
// 1-minute load swinging around 144 — 1.5 per core, host.load_per_core_max
// — all day, every few minutes over and under, and node.log carrying
// hundreds of `capacity N → M: host hot: load X/96 cores` lines. Under
// that load the node user's slice was not starved: its cpu.pressure some
// avg60 is modelled at 8–20%, under host.cpu_some_avg60_max (25). One
// afternoon spell is really hot — 40% with dips to 22%, inside the quiet
// band — for forty minutes.

const replayTick = 30 * time.Second

// dl360Day is the reading at d into the day: the load around 144 on 96
// cores (a seven-minute swing and a faster ripple), the slice's pressure
// following it under 25%, and the hot spell from 14:00 to 14:40.
func dl360Day(d time.Duration) MachineReading {
	m := d.Minutes()
	load := 144 + 22*math.Sin(2*math.Pi*m/7) + 6*math.Sin(2*math.Pi*m/1.5)
	pressure := 14 + 6*math.Sin(2*math.Pi*m/7)
	if d >= 14*time.Hour && d < 14*time.Hour+40*time.Minute {
		pressure = 40
		if int(m)%5 == 0 {
			pressure = 22
		}
	}
	return MachineReading{Load1: load, CPUs: 96, MemAvailable: 64 << 30,
		SliceCPUSomeAvg60: pressure, HasSliceCPUPressure: true}
}

func TestReplayDl360DayCapacityHoldsStill(t *testing.T) {
	// the series is the flapping one: under ADR-023's rule — load per
	// core past 1.5, a slot every tick — it was hot on hundreds of ticks
	oldHot := 0
	for d := time.Duration(0); d < 24*time.Hour; d += replayTick {
		if m := dl360Day(d); m.PerCore() > DefaultLoadPerCoreMax {
			oldHot++
		}
	}
	if oldHot < 500 {
		t.Fatalf("the series is hot on %d ticks by load per core, want the flapping day's hundreds", oldHot)
	}

	probe := newHostProbe(0)
	s := hostScaled(t, Capacity{Min: 2, Max: 8}, 8, probe)
	low, recovered := 8, false
	for d := time.Duration(0); d < 24*time.Hour; d += replayTick {
		probe.mu.Lock()
		probe.m = dl360Day(d)
		probe.mu.Unlock()
		s.at(d)
		n := s.loop.Capacity()
		if d < 14*time.Hour && n != 8 {
			t.Fatalf("%s: capacity %d before the hot spell, want 8 (load %.0f/96, pressure %.0f%%)\n%s",
				d, n, dl360Day(d).Load1, dl360Day(d).SliceCPUSomeAvg60, s.logs)
		}
		if d >= 14*time.Hour && d < 14*time.Hour+40*time.Minute {
			low = min(low, n)
			if n < 8 && !s.loop.hostHot() {
				t.Fatalf("%s: quiet inside the hot spell (pressure %.0f%%)", d, dl360Day(d).SliceCPUSomeAvg60)
			}
		}
		if d >= 15*time.Hour+30*time.Minute && n == 8 {
			recovered = true
		}
	}
	if low != 2 {
		t.Fatalf("the hot spell backed off to %d, want the floor 2\n%s", low, s.logs)
	}
	if !recovered || s.loop.Capacity() != 8 {
		t.Fatalf("not recovered after the hot spell: capacity %d\n%s", s.loop.Capacity(), s.logs)
	}

	var lines []string
	for _, l := range strings.Split(s.logs.String(), "\n") {
		if strings.Contains(l, "capacity ") && strings.Contains(l, "→") {
			lines = append(lines, l)
		}
	}
	if len(lines) >= 20 {
		t.Fatalf("%d capacity lines over the day, want fewer than 20:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(s.logs.String(), "capacity 8 → 7: host hot: cpu pressure 40%/25%") ||
		!strings.Contains(s.logs.String(), "capacity 7 → 8: host quiet 5m") {
		t.Fatalf("capacity lines:\n%s", strings.Join(lines, "\n"))
	}
}
