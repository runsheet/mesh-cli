package node

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

type fakeMachine struct {
	m   MachineReading
	err error
}

func (f fakeMachine) Read() (MachineReading, error) { return f.m, f.err }

var headroomT0 = time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)

// feed runs the scaler for half an hour, a sample a minute with
// slotsRunning slots, each window's utilization rising linearly from
// its start to its end, resetting at the given times; a nil range
// leaves the window unread.
type rise struct {
	from, to float64
	resetsAt time.Time
}

func feed(s *Scaler, c *fakeClock, slotsRunning float64, five, seven *rise) {
	for i := 0; i <= 30; i++ {
		f := float64(i) / 30
		var ws Windows
		if five != nil {
			ws.FiveHour = &WindowReading{Utilization: five.from + (five.to-five.from)*f, ResetsAt: five.resetsAt}
		}
		if seven != nil {
			ws.SevenDay = &WindowReading{Utilization: seven.from + (seven.to-seven.from)*f, ResetsAt: seven.resetsAt}
		}
		slotMinutes := slotsRunning
		if i == 0 {
			slotMinutes = 0
		}
		s.Observe(ws, slotMinutes)
		if i < 30 {
			c.t = c.t.Add(time.Minute)
		}
	}
}

func newTestScaler(min, max int, m MachineProbe) (*Scaler, *fakeClock) {
	c := &fakeClock{t: headroomT0}
	return NewScaler(Capacity{Min: min, Max: max}, c.Now, m), c
}

// (1) Well under weekly pace — five days left, a third of the week spent,
// two slots moving it a tenth of a percent in half an hour — the windows
// afford more than max: the target is max, for reason max.
func TestScalerUnderWeeklyPaceReachesMax(t *testing.T) {
	s, c := newTestScaler(1, 4, nil)
	feed(s, c, 2,
		&rise{0.10, 0.101, headroomT0.Add(4 * time.Hour)},
		&rise{0.30, 0.301, headroomT0.Add(5 * 24 * time.Hour)})
	if n, why := s.Target(2); n != 4 || why != ReasonMax {
		t.Fatalf("target = %d (%s), want 4 (max)", n, why)
	}
}

// (2) Over weekly pace — four fifths spent with five days to go, two
// slots spending two percent in half an hour — the target is min, for
// reason weekly pace.
func TestScalerOverWeeklyPaceFallsToMin(t *testing.T) {
	s, c := newTestScaler(1, 4, nil)
	feed(s, c, 2,
		&rise{0.10, 0.101, headroomT0.Add(4 * time.Hour)},
		&rise{0.80, 0.82, headroomT0.Add(5 * 24 * time.Hour)})
	if n, why := s.Target(2); n != 1 || why != ReasonWeeklyPace {
		t.Fatalf("target = %d (%s), want 1 (weekly pace)", n, why)
	}
}

// (3) The five-hour window near its 0.90 target caps the target below
// what the seven-day window alone would allow.
func TestScalerFiveHourNearTargetCaps(t *testing.T) {
	seven := &rise{0.30, 0.301, headroomT0.Add(5 * 24 * time.Hour)}

	weekOnly, c := newTestScaler(1, 8, nil)
	feed(weekOnly, c, 2, nil, seven)
	weekly, why := weekOnly.Target(2)
	if why != ReasonWeeklyPace || weekly != 5 {
		t.Fatalf("seven-day alone: %d (%s), want 5 (weekly pace)", weekly, why)
	}

	s, c := newTestScaler(1, 8, nil)
	// 0.865 of the window, two hours to its reset, half a percent per
	// half hour of two slots: (0.90-0.865)/120 min / (0.005/60) = 3.5
	feed(s, c, 2, &rise{0.86, 0.865, headroomT0.Add(30*time.Minute + 2*time.Hour)}, seven)
	n, why := s.Target(2)
	if n != 3 || why != ReasonFiveHourPace {
		t.Fatalf("target = %d (%s), want 3 (five-hour pace)", n, why)
	}
	if n >= weekly {
		t.Fatalf("five-hour target %d is not below the seven-day one %d", n, weekly)
	}
}

// (4) With no burn data the default burn is used; a half hour in which
// utilization did not move reads as the floor, not as free slots.
func TestScalerDefaultBurnAndFloor(t *testing.T) {
	seven := &WindowReading{Utilization: 0.35, ResetsAt: headroomT0.Add(600 * time.Minute)}

	s, _ := newTestScaler(1, 8, nil)
	s.Observe(Windows{SevenDay: seven}, 0)
	if b := s.burn(sevenDay); b != DefaultBurnSevenDay {
		t.Fatalf("burn with one sample = %v, want the default %v", b, DefaultBurnSevenDay)
	}
	// (0.95-0.35)/600 min / (1/2400) = 2.4
	if n, why := s.Target(0); n != 2 || why != ReasonWeeklyPace {
		t.Fatalf("target on the default burn = %d (%s), want 2 (weekly pace)", n, why)
	}

	// slot-minutes ran but no window moved: the floor, and max
	s, c := newTestScaler(1, 8, nil)
	feed(s, c, 2, nil, &rise{0.35, 0.35, seven.ResetsAt})
	if b := s.burn(sevenDay); b != FloorBurnSevenDay {
		t.Fatalf("burn with no rise = %v, want the floor %v", b, FloorBurnSevenDay)
	}
	if n, why := s.Target(2); n != 8 || why != ReasonMax {
		t.Fatalf("target on the floor = %d (%s), want 8 (max)", n, why)
	}

	// samples but no slot ran: no data, the default again
	s, c = newTestScaler(1, 8, nil)
	feed(s, c, 0, nil, &rise{0.35, 0.36, seven.ResetsAt})
	if b := s.burn(sevenDay); b != DefaultBurnSevenDay {
		t.Fatalf("burn with no slot-minutes = %v, want the default", b)
	}

	// a window that reset inside the half hour is no data either
	s, c = newTestScaler(1, 8, nil)
	s.Observe(Windows{SevenDay: &WindowReading{Utilization: 0.9, ResetsAt: headroomT0.Add(time.Minute)}}, 0)
	c.t = c.t.Add(10 * time.Minute)
	s.Observe(Windows{SevenDay: &WindowReading{Utilization: 0.01, ResetsAt: headroomT0.Add(7 * 24 * time.Hour)}}, 20)
	if b := s.burn(sevenDay); b != DefaultBurnSevenDay {
		t.Fatalf("burn across a reset = %v, want the default", b)
	}

	// history older than half an hour is dropped
	c.t = c.t.Add(BurnWindow + time.Minute)
	s.Observe(Windows{}, 0)
	if len(s.samples) != 1 {
		t.Fatalf("kept %d samples, want only the latest", len(s.samples))
	}
}

// (5) Step moves by at most one, and not again within five minutes.
func TestStepOneAtATime(t *testing.T) {
	now := headroomT0
	n, moved := Step(1, 4, time.Time{}, now)
	if n != 2 || !moved {
		t.Fatalf("first step = %d (%v), want 2", n, moved)
	}
	last := now
	if n, moved = Step(n, 4, last, now.Add(4*time.Minute+59*time.Second)); n != 2 || moved {
		t.Fatalf("within five minutes = %d (%v), want 2 unmoved", n, moved)
	}
	if n, moved = Step(n, 4, last, now.Add(StepInterval)); n != 3 || !moved {
		t.Fatalf("after five minutes = %d (%v), want 3", n, moved)
	}
	if n, moved = Step(4, 1, time.Time{}, now); n != 3 || !moved {
		t.Fatalf("down = %d (%v), want 3", n, moved)
	}
	if n, moved = Step(2, 2, time.Time{}, now); n != 2 || moved {
		t.Fatalf("at target = %d (%v), want 2 unmoved", n, moved)
	}
}

// (6) A busy machine — load at the CPU count, or 2 GB free or less —
// caps the target at the sessions already running, for machine load;
// an idle one, or a probe that cannot read, caps nothing.
func TestScalerMachineLoadCaps(t *testing.T) {
	under := func(m MachineProbe) *Scaler {
		s, c := newTestScaler(1, 4, m)
		feed(s, c, 2,
			&rise{0.10, 0.101, headroomT0.Add(4 * time.Hour)},
			&rise{0.30, 0.301, headroomT0.Add(5 * 24 * time.Hour)})
		return s
	}
	idle := MachineReading{Load1: 1.5, CPUs: 8, MemAvailable: 8 << 30}
	for _, tc := range []struct {
		name string
		m    MachineProbe
		want int
		why  Reason
	}{
		{"idle", fakeMachine{m: idle}, 4, ReasonMax},
		{"high load", fakeMachine{m: MachineReading{Load1: 8.2, CPUs: 8, MemAvailable: 8 << 30}}, 2, ReasonMachineLoad},
		{"under 2 GB free", fakeMachine{m: MachineReading{Load1: 1, CPUs: 8, MemAvailable: 1 << 30}}, 2, ReasonMachineLoad},
		{"unreadable", fakeMachine{err: errors.New("no /proc")}, 4, ReasonMax},
	} {
		if n, why := under(tc.m).Target(2); n != tc.want || why != tc.why {
			t.Errorf("%s: target = %d (%s), want %d (%s)", tc.name, n, why, tc.want, tc.why)
		}
	}
	// running above what the windows afford: the windows' target stands
	busy := fakeMachine{m: MachineReading{Load1: 9, CPUs: 8, MemAvailable: 8 << 30}}
	s, c := newTestScaler(1, 4, busy)
	feed(s, c, 2, nil, &rise{0.80, 0.82, headroomT0.Add(5 * 24 * time.Hour)})
	if n, why := s.Target(3); n != 1 || why != ReasonWeeklyPace {
		t.Fatalf("busy machine over pace = %d (%s), want 1 (weekly pace)", n, why)
	}
}

// ProcMachine reads the load and MemAvailable from /proc.
func TestProcMachine(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/proc/loadavg", "3.47 2.10 1.90 2/812 12345\n")
	writeFile(t, root+"/proc/meminfo", "MemTotal:       16318548 kB\nMemFree:          512000 kB\nMemAvailable:    1048576 kB\n")
	pm := ProcMachine{Root: root}
	m, err := pm.Read()
	if err != nil {
		t.Fatal(err)
	}
	if m.Load1 != 3.47 || m.MemAvailable != 1<<30 || m.CPUs < 1 {
		t.Fatalf("read %+v", m)
	}
	if !m.Busy() {
		t.Fatal("1 GB available is busy")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
