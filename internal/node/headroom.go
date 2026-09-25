package node

import (
	"math"
	"time"
)

// The headroom scaler: how many items the node should run at once,
// between the min and the max of node.yaml's capacity, by how much of
// the plan's five-hour and seven-day windows is left and how fast the
// node has been spending them. It is pure — no I/O, the clock passed
// in — so every rule below is tested with simulated windows.

const (
	// historySpan is how far back the burn is measured: the rise of a
	// window over the last half hour, against the slot-minutes run in it.
	historySpan = 30 * time.Minute
	// minSlotMinutes is how much running the history must hold before
	// its rise is believed; with less, one session's first minutes (or
	// an idle node) would read as a burn of nothing, or of everything.
	minSlotMinutes = 10.0
	// burnDefault is the five-hour utilization one slot spends a minute
	// when there is no measure yet: a busy session takes about a fifth
	// of the five-hour window an hour. It is cautious on purpose — a
	// node without data starts low and climbs as the data shows room.
	burnDefault = 0.20 / 60
	// burnFloor is the least five-hour burn per slot-minute ever
	// assumed, so a quiet half hour (a session waiting on CI) does not
	// read as limitless room: a fiftieth of the default.
	burnFloor = burnDefault / 50
	// sevenDayScale turns a five-hour burn into a seven-day one for the
	// default and the floor: the week holds about ten five-hour windows
	// of work, so a slot-minute spends a tenth as much of it.
	sevenDayScale = 0.1
	// fiveHourTarget is where the five-hour window is aimed at its
	// reset: a little short of the hard stop, so the last items of a
	// window are not cut off mid-session.
	fiveHourTarget = 0.90
	// sevenDayTarget is where the week is aimed at its reset: spread
	// over the week, not spent on its first day, with a margin.
	sevenDayTarget = 0.95
	// stepEvery is the least time between two steps, so a step's effect
	// shows in the windows before the next is taken.
	stepEvery = 5 * time.Minute
	// minFreeBytes is the free memory under which the machine is loaded
	// (CI runs on the same mini).
	minFreeBytes = 2 << 30
)

// The reasons Decide gives for the capacity it chose.
const (
	ReasonFiveHour = "five-hour pace"
	ReasonWeekly   = "weekly pace"
	ReasonLoad     = "machine load"
	ReasonMax      = "max"
)

// Window is one rate-limit window as last reported: how much of it is
// used (0–1) and when it resets.
type Window struct {
	Utilization float64
	ResetsAt    time.Time
}

// MachineLoad is what the machine can take: the 1-minute load average
// against the CPU count, and the free memory. A zero CPUs or FreeBytes
// is not known and does not count.
type MachineLoad struct {
	Load1     float64
	CPUs      int
	FreeBytes uint64
}

// Observation is one sample: the windows (nil when not reported), the
// slots running, and the machine's load, at a time.
type Observation struct {
	At                 time.Time
	FiveHour, SevenDay *Window
	Running            int
	Load               MachineLoad
}

// Scaler keeps the last half hour of observations and moves the
// capacity one step at a time toward what the windows can afford.
type Scaler struct {
	history  []Observation
	lastStep time.Time
}

// Observe records a sample and drops those older than the history span.
func (s *Scaler) Observe(obs Observation) {
	s.history = append(s.history, obs)
	cut := 0
	for cut < len(s.history) && obs.At.Sub(s.history[cut].At) > historySpan {
		cut++
	}
	s.history = s.history[cut:]
}

// Decide is the capacity to run now, given the one in force and the
// range, with the reason for its target. It moves one step toward the
// target, and only when stepEvery has passed since the last step;
// otherwise it stays.
func (s *Scaler) Decide(now time.Time, current, min, max int) (next int, reason string) {
	if current < min {
		current = min
	}
	if current > max {
		current = max
	}
	target, reason := s.target(now, current, min, max)
	if target == current {
		return current, reason
	}
	if !s.lastStep.IsZero() && now.Sub(s.lastStep) < stepEvery {
		return current, reason
	}
	s.lastStep = now
	if target > current {
		return current + 1, reason
	}
	return current - 1, reason
}

// target is the capacity the windows and the machine allow, in [min, max].
func (s *Scaler) target(now time.Time, current, min, max int) (int, string) {
	slots5 := s.slots(now, fiveHour, fiveHourTarget, burnDefault, burnFloor)
	slots7 := s.slots(now, sevenDay, sevenDayTarget, burnDefault*sevenDayScale, burnFloor*sevenDayScale)
	target, reason := slots7, ReasonWeekly
	if slots5 < slots7 {
		target, reason = slots5, ReasonFiveHour
	}
	if target >= max {
		target, reason = max, ReasonMax
	}
	if target < min {
		target = min
	}
	if s.loaded() {
		ceiling := current - 1
		if ceiling < min {
			ceiling = min
		}
		if target > ceiling {
			target = ceiling
		}
		reason = ReasonLoad
	}
	return target, reason
}

func fiveHour(o Observation) *Window { return o.FiveHour }
func sevenDay(o Observation) *Window { return o.SevenDay }

// slots is how many slots one window can afford: the rate it can spend
// until its reset, (target − utilization) ÷ minutes left, over the burn
// of one slot. A window not reported, or past its reset, is
// unconstrained (math.MaxInt); one at or over its target affords none.
func (s *Scaler) slots(now time.Time, win func(Observation) *Window, target, def, floor float64) int {
	if len(s.history) == 0 {
		return math.MaxInt
	}
	w := win(s.history[len(s.history)-1])
	if w == nil {
		return math.MaxInt
	}
	left := w.ResetsAt.Sub(now).Minutes()
	if left <= 0 {
		return math.MaxInt
	}
	afford := (target - w.Utilization) / left
	if afford <= 0 {
		return 0
	}
	n := math.Floor(afford / s.burn(now, win, def, floor))
	if n >= math.MaxInt32 {
		return math.MaxInt
	}
	return int(n)
}

// burn is one window's utilization spent per slot-minute over the
// history: its rise since the last reset, over the slot-minutes run
// since (each sample's Running held until the next). Samples before a
// reset (the utilization fell) or a gap in the window are dropped.
// Fewer than minSlotMinutes, or no rise, is def; it is never under floor.
func (s *Scaler) burn(now time.Time, win func(Observation) *Window, def, floor float64) float64 {
	var h []Observation
	for _, o := range s.history {
		if now.Sub(o.At) <= historySpan && !o.At.After(now) {
			h = append(h, o)
		}
	}
	start := 0
	for i := range h {
		w := win(h[i])
		if w == nil {
			start = i + 1
			continue
		}
		if i > start && w.Utilization < win(h[i-1]).Utilization {
			start = i
		}
	}
	if len(h)-start < 2 {
		return def
	}
	h = h[start:]
	var slotMinutes float64
	for i := 1; i < len(h); i++ {
		slotMinutes += float64(h[i-1].Running) * h[i].At.Sub(h[i-1].At).Minutes()
	}
	rise := win(h[len(h)-1]).Utilization - win(h[0]).Utilization
	if slotMinutes < minSlotMinutes || rise <= 0 {
		return def
	}
	return math.Max(rise/slotMinutes, floor)
}

// loaded says the machine can take no more: the 1-minute load at or
// over the CPU count, or free memory under minFreeBytes.
func (s *Scaler) loaded() bool {
	if len(s.history) == 0 {
		return false
	}
	l := s.history[len(s.history)-1].Load
	return (l.CPUs > 0 && l.Load1 >= float64(l.CPUs)) || (l.FreeBytes > 0 && l.FreeBytes < minFreeBytes)
}
