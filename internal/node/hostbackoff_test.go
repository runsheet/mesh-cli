package node

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// bare is s without its host report, for comparing the rest.
func bare(s Scaling) Scaling { s.Host = nil; return s }

// hostProbe is a MachineProbe the test sets: 18 cores, plenty of memory,
// the load and temperature as given.
type hostProbe struct {
	mu sync.Mutex
	m  MachineReading
}

func newHostProbe(load float64) *hostProbe {
	return &hostProbe{m: MachineReading{Load1: load, CPUs: 18, MemAvailable: 16 << 30}}
}

func (h *hostProbe) Read() (MachineReading, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.m, nil
}

func (h *hostProbe) set(load float64, tempC float64, hasTemp bool) {
	h.mu.Lock()
	h.m.Load1, h.m.PackageTempC, h.m.HasTemp = load, tempC, hasTemp
	h.mu.Unlock()
}

// hostScaled is a scaled node of capacity c in force at start, on probe.
func hostScaled(t *testing.T, c Capacity, start int, probe MachineProbe) *scaled {
	s := newScaled(t, c, false)
	s.loop.Machine = probe
	s.loop.capacity = start
	return s
}

// A ranged {2, 8} node at 8 on a host at 3.0 load per core drops one slot
// per tick to its min and claims nothing; at 0.5 per core it holds for
// five quiet minutes, then climbs one slot per five back to 8 and no
// further, its reason the plan's again.
func TestHostBackoffShrinksPerTickAndClimbsPerFiveQuietMinutes(t *testing.T) {
	probe := newHostProbe(54)
	s := hostScaled(t, Capacity{Min: 2, Max: 8}, 8, probe)
	s.addGoal(orgID, "hot", "urgent")
	s.addItem(orgID, "hot", "h1", 1, false)

	got := []int{s.loop.Capacity()}
	var claimed []string
	for m := 0; m <= 6; m++ {
		claimed = append(claimed, s.at(time.Duration(m)*time.Minute)...)
		got = append(got, s.loop.Capacity())
		sc, ok := s.loop.Scaling()
		if !ok || sc.Reason != "host hot: load 54/18 cores" {
			t.Fatalf("+%dm: scaling %+v, %v", m, sc, ok)
		}
		if sc.Host == nil || *sc.Host != (HostReport{Load: 54, Cores: 18, Hot: true}) {
			t.Fatalf("+%dm: host %+v", m, sc.Host)
		}
	}
	if want := []int{8, 7, 6, 5, 4, 3, 2, 2}; !equalInts(got, want) {
		t.Fatalf("hot: capacities %v, want %v", got, want)
	}
	if len(claimed) != 0 {
		t.Fatalf("a hot host claimed %v", claimed)
	}
	if !strings.Contains(s.logs.String(), "capacity 8 → 7: host hot: load 54/18 cores\n") ||
		!strings.Contains(s.logs.String(), "capacity 3 → 2: host hot: load 54/18 cores\n") ||
		strings.Contains(s.logs.String(), "capacity 2 → 1") {
		t.Fatalf("logs:\n%s", s.logs)
	}

	// quiet from +7m
	probe.set(9, 0, false)
	quiet := 7 * time.Minute
	at := func(d time.Duration) int {
		s.at(quiet + d)
		s.loop.wg.Wait()
		return s.loop.Capacity()
	}
	for _, d := range []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute, 4*time.Minute + 59*time.Second} {
		if n := at(d); n != 2 {
			t.Fatalf("quiet +%s: capacity %d, want 2", d, n)
		}
	}
	if sc, _ := s.loop.Scaling(); sc.Host == nil || sc.Host.Hot {
		t.Fatalf("quiet: host %+v", sc.Host)
	}
	if n := at(5 * time.Minute); n != 3 {
		t.Fatalf("quiet +5m: capacity %d, want 3", n)
	}
	for m := 6; m <= 40; m++ {
		d := time.Duration(m) * time.Minute
		n := at(d)
		want := min(3+(m-5)/5, 8)
		if n != want {
			t.Fatalf("quiet +%s: capacity %d, want %d", d, n, want)
		}
		if m == 30 && n != 8 {
			t.Fatalf("quiet +30m: capacity %d, want 8", n)
		}
	}
	sc, _ := s.loop.Scaling()
	if sc.Capacity != 8 || strings.HasPrefix(string(sc.Reason), "host hot") || sc.Reason != ReasonMax {
		t.Fatalf("level again: scaling %+v", sc)
	}
	if !strings.Contains(s.logs.String(), "capacity 2 → 3: host quiet 5m\n") ||
		!strings.Contains(s.logs.String(), "capacity 7 → 8: host quiet 5m\n") ||
		strings.Contains(s.logs.String(), "capacity 8 → 9") {
		t.Fatalf("logs:\n%s", s.logs)
	}
	if len(s.CallsNamed("claimWorkItem")) == 0 {
		t.Fatal("a quiet host claimed nothing")
	}
}

// A fixed `capacity: 4` node backs off to 1 and climbs back to 4; its
// scaling is reported while held back, and not once level and quiet.
func TestHostBackoffFixedNode(t *testing.T) {
	probe := newHostProbe(54)
	s := hostScaled(t, Fixed(4), 4, probe)
	if _, ok := s.loop.Scaling(); ok {
		t.Fatal("a fixed node reports scaling before any reading")
	}
	var got []int
	for m := 0; m < 5; m++ {
		s.at(time.Duration(m) * time.Minute)
		got = append(got, s.loop.Capacity())
	}
	if want := []int{3, 2, 1, 1, 1}; !equalInts(got, want) {
		t.Fatalf("hot: capacities %v, want %v", got, want)
	}
	if sc, ok := s.loop.Scaling(); !ok || bare(sc) != (Scaling{Capacity: 1, Min: 1, Max: 4, Reason: "host hot: load 54/18 cores"}) {
		t.Fatalf("fixed, hot: %+v, %v", sc, ok)
	}
	if hb := s.loop.heartbeatScaling(s.loop.Scaling()); hb.Capacity != 1 || hb.Reason != "host hot: load 54/18 cores" {
		t.Fatalf("heartbeat scaling %+v", hb)
	}
	probe.set(9, 0, false)
	for m := 5; m <= 20; m++ {
		s.at(time.Duration(m) * time.Minute)
		s.loop.wg.Wait()
	}
	if n := s.loop.Capacity(); n != 4 {
		t.Fatalf("quiet 15m: capacity %d, want 4", n)
	}
	if _, ok := s.loop.Scaling(); ok {
		t.Fatal("a fixed node level and quiet still reports scaling")
	}
	if hb := s.loop.heartbeatScaling(s.loop.Scaling()); hb.Reason != string(ReasonFixed) || hb.Capacity != 4 {
		t.Fatalf("heartbeat scaling %+v", hb)
	}
}

// Temperature alone — load quiet — shrinks the same way.
func TestHostBackoffTemperatureAlone(t *testing.T) {
	probe := newHostProbe(9)
	probe.set(9, 101, true)
	s := hostScaled(t, Capacity{Min: 2, Max: 8}, 8, probe)
	s.at(0)
	s.at(time.Minute)
	sc, _ := s.loop.Scaling()
	if sc.Capacity != 6 || sc.Reason != "host hot: 101°C" || sc.Host == nil || !sc.Host.HasTemp || sc.Host.TempC != 101 {
		t.Fatalf("101°C: scaling %+v host %+v", sc, sc.Host)
	}
	if !strings.Contains(s.logs.String(), "capacity 8 → 7: host hot: 101°C\n") {
		t.Fatalf("logs:\n%s", s.logs)
	}
}

// No machine probe (a host other than Linux): the capacity never moves.
func TestHostBackoffNilMachine(t *testing.T) {
	s := hostScaled(t, Capacity{Min: 2, Max: 8}, 8, nil)
	f := hostScaled(t, Fixed(4), 4, nil)
	for m := 0; m < 10; m++ {
		s.at(time.Duration(m) * time.Minute)
		f.at(time.Duration(m) * time.Minute)
	}
	if sc, _ := s.loop.Scaling(); sc.Capacity != 8 || sc.Host != nil || sc.Reason != ReasonMax {
		t.Fatalf("ranged, no machine: %+v", sc)
	}
	if _, ok := f.loop.Scaling(); ok || f.loop.Capacity() != 4 {
		t.Fatalf("fixed, no machine: capacity %d", f.loop.Capacity())
	}
}

// hostState.report() carries the node user's own slice's CPU pressure
// avg60 alongside the host-wide reading.
func TestHostStateReportCarriesSliceCPUPressure(t *testing.T) {
	h := hostState{read: true, reading: MachineReading{
		Load1: 10, CPUs: 18, SliceCPUSomeAvg60: 42, HasSliceCPUPressure: true,
	}}
	r := h.report()
	if r == nil || !r.HasSliceCPUPressure || r.SliceCPUSomeAvg60 != 42 {
		t.Fatalf("report() = %+v, want HasSliceCPUPressure true, SliceCPUSomeAvg60 42", r)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
