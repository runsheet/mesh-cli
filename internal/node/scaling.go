package node

import (
	"time"

	"projectmesh.io/mesh-cli/internal/harness"
)

// The loop's half of adr-004: a node whose node.yaml capacity is a range
// runs the Scaler once a minute and steps the capacity in force toward
// its target. A fixed capacity runs none of this.

// ScaleEvery is how often a ranged node evaluates its scaler, whatever
// the loop's interval.
const ScaleEvery = time.Minute

// WindowSource gives the plan's windows as last read; false is no
// reading at all, which sets no bound.
type WindowSource func() (Windows, bool)

// Scaling is a ranged node's capacity in force, its range and why it is
// what it is — what the heartbeat's `scaling` carries.
type Scaling struct {
	Capacity int
	Min, Max int
	Reason   Reason
}

// scaleState is the loop's scaler and when it last evaluated and moved.
type scaleState struct {
	scaler    *Scaler
	evaluated time.Time
	stepped   time.Time
	reason    Reason
}

// rangeLocked is node.yaml's capacity as last read; l.mu is held.
func (l *Loop) rangeLocked() Capacity {
	if l.capRange.Max > 0 {
		return l.capRange
	}
	return l.Cfg.Capacity
}

// Scaling is the capacity in force, the range and the reason, and true,
// when node.yaml's capacity is a range; a fixed node has none.
func (l *Loop) Scaling() (Scaling, bool) {
	capacity := l.Capacity()
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.rangeLocked()
	if !r.IsRange() {
		return Scaling{}, false
	}
	return Scaling{Capacity: capacity, Min: r.Min, Max: r.Max, Reason: l.scaling.reason}, true
}

// atMin says a ranged node is at (or, held by the machine, below) its
// min: it claims only urgent and high items there.
func (l *Loop) atMin() bool {
	s, ok := l.Scaling()
	return ok && s.Capacity <= s.Min
}

// setRange takes node.yaml's capacity as read again. A fixed one is in
// force as it is; a range clamps the capacity in force into it at once,
// and the scaler goes on from there.
func (l *Loop) setRange(c Capacity) {
	was := l.Capacity()
	l.mu.Lock()
	old := l.rangeLocked()
	l.capRange = c
	l.mu.Unlock()
	if !c.IsRange() {
		if c.Max != was {
			l.logf("capacity %d → %d", was, c.Max)
		}
		l.mu.Lock()
		l.capacity, l.scaling = c.Max, scaleState{}
		l.mu.Unlock()
		return
	}
	if c == old {
		return
	}
	now := min(max(was, c.Min), c.Max)
	l.logf("capacity %s → %s", old, c)
	if now != was {
		l.logf("capacity %d → %d: clamped into %s", was, now, c)
	}
	l.mu.Lock()
	l.capacity = now
	if now == c.Max && now < was {
		l.scaling.reason = ReasonMax
	}
	l.mu.Unlock()
}

// scale runs a ranged node's scaler, at most once every ScaleEvery by
// the loop's clock: it records the slot-minutes run since the last run
// (Running × minutes), feeds it the windows as read now, and steps the
// capacity in force toward the target (Step: one at a time, not within
// StepInterval of the last). A shrink below Running stops claiming and
// nothing else. The hard stops of pace stay above all of this.
func (l *Loop) scale() {
	now := l.now()
	l.mu.Lock()
	r := l.rangeLocked()
	if !r.IsRange() {
		l.scaling = scaleState{}
		l.mu.Unlock()
		return
	}
	st := &l.scaling
	if st.scaler == nil {
		// a new ranged node starts at its min; one that was fixed keeps
		// what it had, clamped (setRange). Either counts as a step.
		st.scaler = NewScaler(r, l.now, l.Machine)
		if l.capacity <= 0 {
			l.capacity = r.Min
		}
		st.stepped = now
		l.logf("capacity %s: scaling by the plan's windows from %d", r, l.capacity)
	}
	st.scaler.Min, st.scaler.Max = r.Min, r.Max
	if !st.evaluated.IsZero() && now.Sub(st.evaluated) < ScaleEvery {
		l.mu.Unlock()
		return
	}
	running := len(l.running)
	var slotMinutes float64
	if !st.evaluated.IsZero() {
		slotMinutes = float64(running) * now.Sub(st.evaluated).Minutes()
	}
	st.evaluated = now
	scaler, current, stepped := st.scaler, l.capacity, st.stepped
	l.mu.Unlock()

	ws, _ := l.windows()
	scaler.Observe(ws, slotMinutes)
	target, reason := scaler.Target(running)
	next, moved := Step(current, target, stepped, now)

	l.mu.Lock()
	st.reason = reason
	if moved {
		l.capacity, st.stepped = next, now
	}
	l.mu.Unlock()
	if moved {
		l.logf("capacity %d → %d: %s (target %d, %d running)", current, next, reason, target, running)
	}
}

// windows are the plan's windows as read now: Windows when set, else
// the limits store's.
func (l *Loop) windows() (Windows, bool) {
	if l.Windows != nil {
		return l.Windows()
	}
	return windowsOf(l.limitsStore().Readings())
}

// windowsOf is the highest utilization across the accounts' readings,
// per window: the account nearest its limit sets the pace.
func windowsOf(readings map[string]harness.RateLimit) (Windows, bool) {
	var ws Windows
	for _, rl := range readings {
		ws.FiveHour = higher(ws.FiveHour, rl.FiveHour)
		ws.SevenDay = higher(ws.SevenDay, rl.SevenDay)
	}
	return ws, ws.FiveHour != nil || ws.SevenDay != nil
}

// higher is the more used of held and w (of two alike, the later reset);
// an empty w is no reading.
func higher(held *WindowReading, w harness.RateLimitWindow) *WindowReading {
	if w == (harness.RateLimitWindow{}) {
		return held
	}
	if held != nil && (held.Utilization > w.Utilization ||
		held.Utilization == w.Utilization && !w.ResetsAt.After(held.ResetsAt)) {
		return held
	}
	return &WindowReading{Utilization: w.Utilization, ResetsAt: w.ResetsAt}
}
