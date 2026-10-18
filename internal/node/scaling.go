package node

import (
	"fmt"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
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
// what it is — what the heartbeat's `scaling` carries — and the host as
// last read (nil before a reading, or with no Machine).
type Scaling struct {
	Capacity int         `json:"capacity"`
	Min      int         `json:"min"`
	Max      int         `json:"max"`
	Reason   Reason      `json:"reason"`
	Host     *HostReport `json:"host,omitempty"`
}

// Heartbeat is s as the heartbeat's `scaling` carries it.
func (s Scaling) Heartbeat() *gateway.Scaling {
	// Accounts is [] rather than nil: the platform refuses a null list
	out := &gateway.Scaling{Capacity: s.Capacity, Min: s.Min, Max: s.Max, Reason: string(s.Reason), Accounts: []gateway.AccountScaling{}}
	if s.Host != nil {
		out.Host = &gateway.HostScaling{Load: s.Host.Load, Cores: s.Host.Cores, Hot: s.Host.Hot, PerCore: s.Host.PerCore()}
		if s.Host.HasTemp {
			out.Host.TemperatureC = s.Host.TempC
		}
		if s.Host.HasCPUPressure {
			out.Host.CPUSomeAvg60 = s.Host.CPUSomeAvg60
		}
	}
	return out
}

// ReasonFixed is a fixed capacity's reason, which the heartbeat's
// `scaling` carries only for a fixed node on several accounts.
const ReasonFixed Reason = "fixed"

// heartbeatScaling is the heartbeat's `scaling` (adr-004,
// adr-005-capacity-across-accounts): s for a ranged node, and each
// account's slots once the accounts scaler has counted them; for a
// fixed node — on any number of accounts, the operator's own login
// too — its capacity in force as capacity, min and max, reason `fixed`,
// so a node.yaml capacity changed under a running node reaches the
// platform on the next heartbeat rather than at the next registration
// (mesh-cli ADR-018). The gateway sends it only when its schema takes
// it. Names and kinds only — never a token file or a key's variable.
func (l *Loop) heartbeatScaling(s Scaling, ranged bool) *gateway.Scaling {
	if !ranged {
		c := l.Capacity()
		s = Scaling{Capacity: c, Min: c, Max: c, Reason: ReasonFixed}
	}
	out := s.Heartbeat()
	slots, _ := l.AccountSlots()
	for _, a := range slots {
		out.Accounts = append(out.Accounts, gateway.AccountScaling{
			Name: a.Name, Kind: a.Kind, Slots: a.Slots.Normal, UrgentOnlySlots: a.Slots.UrgentOnly, Reason: string(a.Reason),
		})
	}
	return out
}

// String is s as `mesh node status` shows it: in-force/min–max (reason).
// A reason not yet known (the scaler has not run) is left out. A Host
// reading, when there is one, is appended as its own clause — `— host
// load <load> / <cores> cores, <temp>°C`, or `no temperature` where the
// host has no thermal zone to read — whether or not it is what backed
// the capacity off, so a reading just climbing back from one still
// shows it (adr-004's amendment 2026-10-03).
func (s Scaling) String() string {
	out := fmt.Sprintf("%d/%d–%d", s.Capacity, s.Min, s.Max)
	if s.Reason != "" {
		out += " (" + string(s.Reason) + ")"
	}
	if s.Host != nil {
		temp := "no temperature"
		if s.Host.HasTemp {
			temp = hostNum(s.Host.TempC) + "°C"
		}
		out += fmt.Sprintf(" — host load %s / %d cores, %s", hostNum(s.Host.Load), s.Host.Cores, temp)
	}
	return out
}

// scaleState is the loop's scaler and when it last evaluated and moved.
type scaleState struct {
	scaler *Scaler
	// accounts is the accounts scaler, run instead of scaler when
	// node.yaml lists accounts; target is its last target (targeted).
	accounts *AccountsScaler
	target   AccountsTarget
	targeted bool
	// pushed is the accounts the push raised at the last run, so its
	// log line is said once per crossing.
	pushed    map[string]bool
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

// Scaling is the capacity in force, the range, the reason and the host,
// and true, when node.yaml's capacity is a range or a hot host holds a
// fixed one back — then its range is 1 to N and its reason the host's;
// a fixed node level and quiet has none. While the back-off holds, its
// reason is the plan's.
func (l *Loop) Scaling() (Scaling, bool) {
	capacity := l.Capacity()
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.rangeLocked()
	host := l.backoff.report()
	if !r.IsRange() {
		if !l.backoff.active() {
			return Scaling{}, false
		}
		return Scaling{Capacity: capacity, Min: 1, Max: r.Max, Reason: l.backoff.reason, Host: host}, true
	}
	reason := l.scaling.reason
	if l.backoff.active() {
		reason = l.backoff.reason
	}
	return Scaling{Capacity: capacity, Min: r.Min, Max: r.Max, Reason: reason, Host: host}, true
}

// atMin says a ranged node is at (or, held by the machine, below) its
// min: it claims only urgent and high items there. A fixed node held
// back by a hot host is never at its min: the back-off is its own rule.
func (l *Loop) atMin() bool {
	l.mu.Lock()
	ranged := l.rangeLocked().IsRange()
	l.mu.Unlock()
	s, _ := l.Scaling()
	return ranged && s.Capacity <= s.Min
}

// setRange takes node.yaml's capacity as read again. A fixed one is in
// force as it is; a range clamps the capacity in force into it at once,
// and the scaler goes on from there.
func (l *Loop) setRange(c Capacity) {
	l.mu.Lock()
	// the plan figure, not what a hot host holds it to: the back-off
	// stays a layer over whatever the plan becomes
	was := l.planLocked()
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
// (Running × minutes), feeds it the windows as read now — or, when
// node.yaml lists accounts, runs the accounts scaler on each account's
// windows, spend and slot-minutes (accountsTarget) — and steps the
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
	l.mu.Unlock()
	// node.yaml's accounts and the issued ones known (scheduled), read
	// outside l.mu: the store keeps its own lock
	accounts := l.scheduled()
	l.mu.Lock()
	if st.scaler == nil {
		// a new ranged node starts at its min; one that was fixed keeps
		// what it had, clamped (setRange). Either counts as a step.
		st.scaler = NewScaler(r, l.now, l.Machine)
		if l.capacity <= 0 {
			l.capacity = r.Min
		}
		st.stepped = now
		if len(accounts) > 0 {
			l.logf("capacity %s: scaling by what its %d account(s) afford from %d", r, len(accounts), l.capacity)
		} else {
			l.logf("capacity %s: scaling by the plan's windows from %d", r, l.capacity)
		}
	}
	st.scaler.Min, st.scaler.Max = r.Min, r.Max
	if !st.evaluated.IsZero() && now.Sub(st.evaluated) < ScaleEvery {
		l.mu.Unlock()
		return
	}
	running := len(l.running)
	var slotMinutes float64
	perAccount := map[string]float64{}
	if !st.evaluated.IsZero() {
		minutes := now.Sub(st.evaluated).Minutes()
		slotMinutes = float64(running) * minutes
		for _, label := range l.runningOn {
			perAccount[label] += minutes
		}
	}
	st.evaluated = now
	scaler, current, stepped := st.scaler, l.capacity, st.stepped
	l.mu.Unlock()

	var target int
	var reason Reason
	if len(accounts) > 0 {
		target, reason = l.accountsTarget(r, accounts, perAccount, running)
	} else {
		ws, _ := l.windows()
		scaler.Observe(ws, slotMinutes)
		scaler.Host = l.HostThresholds()
		target, reason = scaler.Target(running)
		l.mu.Lock()
		st.accounts, st.targeted = nil, false
		l.mu.Unlock()
	}
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
