package node

import (
	"math"
	"time"
)

// The headroom scaler (adr-004): a node whose node.yaml capacity is a
// range {min, max} runs as many slots as its plan's windows can afford,
// measured by how fast its own slots have been spending them.

// Reason says what set a scaled capacity; the heartbeat's `scaling`
// carries it.
type Reason string

const (
	ReasonMax          Reason = "max"
	ReasonWeeklyPace   Reason = "weekly pace"
	ReasonFiveHourPace Reason = "five-hour pace"
	ReasonMachineLoad  Reason = "machine load"
)

const (
	// SevenDayTarget is the share of the seven-day window the node aims
	// to have spent at its reset: below 1, so the week is spread, not
	// spent on day one.
	SevenDayTarget = 0.95
	// FiveHourTarget is the same for the five-hour window.
	FiveHourTarget = 0.90

	// BurnWindow is how far back the burn per slot is measured.
	BurnWindow = 30 * time.Minute
	// StepInterval is the least time between two steps of capacity.
	StepInterval = 5 * time.Minute

	// DefaultBurnFiveHour and DefaultBurnSevenDay are the burn per slot
	// — utilization (0–1) per slot-minute — before there is data: a slot
	// that spends a five-hour window in five hours of running, and a
	// week's in forty. Cautious on purpose: a new node starts near min
	// and grows as its own burn shows there is room.
	DefaultBurnFiveHour = 1.0 / (5 * 60)
	DefaultBurnSevenDay = 1.0 / (40 * 60)
	// FloorBurnFiveHour and FloorBurnSevenDay are the least burn per
	// slot the scaler believes, a hundredth of the defaults: a half hour
	// in which utilization hardly moved must not read as endless slots.
	FloorBurnFiveHour = DefaultBurnFiveHour / 100
	FloorBurnSevenDay = DefaultBurnSevenDay / 100
)

// WindowReading is one window of the plan's limits as last read:
// utilization a fraction of the window (0–1) and when it resets. It is
// the scaler's own shape, filled from whatever parses rate_limit_event.
type WindowReading struct {
	Utilization float64
	ResetsAt    time.Time
}

// Windows are the two windows the scaler paces by; nil is unknown.
type Windows struct {
	FiveHour *WindowReading
	SevenDay *WindowReading
}

type window int

const (
	fiveHour window = iota
	sevenDay
)

func (w window) of(ws Windows) *WindowReading {
	if w == fiveHour {
		return ws.FiveHour
	}
	return ws.SevenDay
}

func (w window) target() float64 {
	if w == fiveHour {
		return FiveHourTarget
	}
	return SevenDayTarget
}

func (w window) defaultBurn() float64 {
	if w == fiveHour {
		return DefaultBurnFiveHour
	}
	return DefaultBurnSevenDay
}

func (w window) floorBurn() float64 {
	if w == fiveHour {
		return FloorBurnFiveHour
	}
	return FloorBurnSevenDay
}

func (w window) reason() Reason {
	if w == fiveHour {
		return ReasonFiveHourPace
	}
	return ReasonWeeklyPace
}

type headroomSample struct {
	at      time.Time
	windows Windows
	// slotMinutes is the slot-minutes run since the sample before.
	slotMinutes float64
}

// Scaler computes a target capacity in [Min, Max] from the samples it is
// fed. It is pure: the clock is Now and the machine is Machine, both
// injected; it neither reads the limits nor changes the loop.
type Scaler struct {
	Min, Max int
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Machine caps the target by the host's load and memory; nil is no cap.
	Machine MachineProbe

	samples []headroomSample
}

// NewScaler scales within a capacity's range.
func NewScaler(c Capacity, now func() time.Time, machine MachineProbe) *Scaler {
	return &Scaler{Min: c.Min, Max: c.Max, Now: now, Machine: machine}
}

func (s *Scaler) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// Observe records the windows as read now and the slot-minutes run since
// the last observation, keeping BurnWindow of history.
func (s *Scaler) Observe(ws Windows, slotMinutes float64) {
	now := s.now()
	s.samples = append(s.samples, headroomSample{at: now, windows: ws, slotMinutes: slotMinutes})
	cut := now.Add(-BurnWindow)
	i := 0
	for i < len(s.samples) && s.samples[i].at.Before(cut) {
		i++
	}
	s.samples = s.samples[i:]
}

// burn is a window's utilization per slot-minute over the history: the
// rise from its first reading to its last, over the slot-minutes run
// after the first. No data — too few readings, no slot-minutes, or the
// window reset in between — is the default; the floor bounds it below.
func (s *Scaler) burn(w window) float64 {
	first, last := -1, -1
	for i, sm := range s.samples {
		if w.of(sm.windows) == nil {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 || last == first {
		return w.defaultBurn()
	}
	a, b := w.of(s.samples[first].windows), w.of(s.samples[last].windows)
	var slotMinutes float64
	for _, sm := range s.samples[first+1 : last+1] {
		slotMinutes += sm.slotMinutes
	}
	rise := b.Utilization - a.Utilization
	if slotMinutes <= 0 || rise < 0 || !a.ResetsAt.Equal(b.ResetsAt) {
		return w.defaultBurn()
	}
	return math.Max(rise/slotMinutes, w.floorBurn())
}

// slots is how many slots a window can afford from now to its reset at
// the burn measured: (target − utilization) / minutes to reset / burn.
// A window with no reading, or past its reset, sets no bound (+Inf).
func (s *Scaler) slots(w window, now time.Time) float64 {
	var last *WindowReading
	for i := len(s.samples) - 1; i >= 0 && last == nil; i-- {
		last = w.of(s.samples[i].windows)
	}
	if last == nil || !last.ResetsAt.After(now) {
		return math.Inf(1)
	}
	room := w.target() - last.Utilization
	if room <= 0 {
		return 0
	}
	minutes := math.Max(last.ResetsAt.Sub(now).Minutes(), 1)
	return room / minutes / s.burn(w)
}

// Target is the capacity the windows afford — the smaller of the two
// windows' slots, clamped to [Min, Max] — then capped by the machine at
// running (no growth) when it is busy, and the reason for it.
func (s *Scaler) Target(running int) (int, Reason) {
	now := s.now()
	target, reason := s.Max, ReasonMax
	best := float64(s.Max)
	for _, w := range []window{sevenDay, fiveHour} {
		if n := math.Floor(s.slots(w, now)); n < best {
			best, reason = n, w.reason()
		}
	}
	if reason != ReasonMax {
		target = max(int(best), s.Min)
	}
	if s.Machine != nil {
		if m, err := s.Machine.Read(); err == nil && m.Busy() && running < target {
			target, reason = max(running, 0), ReasonMachineLoad
		}
	}
	return target, reason
}

// Step moves current toward target by at most one, and not within
// StepInterval of the last change (lastChange zero is never). It says
// whether it moved; a caller that moved records now as lastChange.
func Step(current, target int, lastChange, now time.Time) (int, bool) {
	if current == target || (!lastChange.IsZero() && now.Sub(lastChange) < StepInterval) {
		return current, false
	}
	if target > current {
		return current + 1, true
	}
	return current - 1, true
}
