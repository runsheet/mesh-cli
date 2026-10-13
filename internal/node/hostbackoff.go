package node

import (
	"fmt"
	"time"
)

// The host back-off (docs/adr/adr-023-the-node-backs-off-a-hot-host,
// amended by adr-031-a-sessions-load-matches-its-cpu-limit): a layer
// over the plan figure — the scaler's capacity for a range, N for a
// fixed `capacity: N` — run every tick before claiming. While the
// machine is hot (MachineReading.Hot at node.yaml's `host:` thresholds,
// the slice's CPU pressure when read, with a quiet band below the
// threshold once hot) nothing new is claimed and the capacity in force
// drops one slot per HostStepEvery down to max(min, 1); once it is quiet
// it climbs one slot per HostRecoverEvery of continuous quiet back to
// the plan figure, never above it. Running sessions are never touched.

// HostStepEvery is the least time between two slots the back-off takes
// while the host stays hot: a tick runs more often than a minute, and
// a minute's average needs a minute to answer a step (ADR-031).
const HostStepEvery = time.Minute

// HostRecoverEvery is how long the host must stay quiet for each slot
// the back-off gives back.
const HostRecoverEvery = 5 * time.Minute

// HostReport is the host as the scaling report carries it: the
// 1-minute load, the cores, the package temperature when one was read,
// whether it is past node.yaml's thresholds, and the CPU pressure's
// some avg60 when the host has PSI.
type HostReport struct {
	Load    float64 `json:"load"`
	Cores   int     `json:"cores"`
	TempC   float64 `json:"tempC,omitempty"`
	HasTemp bool    `json:"hasTemp,omitempty"`
	Hot     bool    `json:"hot"`
	// CPUSomeAvg60 is /proc/pressure/cpu's `some avg60`, meaningful only
	// when HasCPUPressure.
	CPUSomeAvg60   float64 `json:"cpuSomeAvg60,omitempty"`
	HasCPUPressure bool    `json:"hasCPUPressure,omitempty"`
	// SliceCPUSomeAvg60 is the node user's own cgroup v2 slice's
	// cpu.pressure `some avg60`, meaningful only when
	// HasSliceCPUPressure.
	SliceCPUSomeAvg60   float64 `json:"sliceCpuSomeAvg60,omitempty"`
	HasSliceCPUPressure bool    `json:"hasSliceCpuPressure,omitempty"`
}

// PerCore is the 1-minute load average per core: Load / Cores, 0 with
// no cores — computed rather than stored, so a report taken before
// this field existed still compares equal by value.
func (h HostReport) PerCore() float64 {
	if h.Cores <= 0 {
		return 0
	}
	return h.Load / float64(h.Cores)
}

// hostState is the back-off as last judged: the reading, whether it was
// hot and why, when the current quiet began (or the last slot was given
// back during it), and how many slots are held back from the plan.
type hostState struct {
	reading    MachineReading
	read       bool
	hot        bool
	reason     Reason
	quietSince time.Time
	// steppedAt is when the back-off last took a slot; the next is
	// HostStepEvery after it.
	steppedAt time.Time
	backedOff int
	// tempAbsentSaid is set once `host: no package temperature zone`
	// has been logged.
	tempAbsentSaid bool
}

// report is the host as last read, nil before a reading.
func (h hostState) report() *HostReport {
	if !h.read {
		return nil
	}
	m := h.reading
	return &HostReport{Load: m.Load1, Cores: m.CPUs, TempC: m.PackageTempC, HasTemp: m.HasTemp, Hot: h.hot,
		CPUSomeAvg60: m.CPUSomeAvg60, HasCPUPressure: m.HasCPUPressure,
		SliceCPUSomeAvg60: m.SliceCPUSomeAvg60, HasSliceCPUPressure: m.HasSliceCPUPressure}
}

// active says the back-off holds the capacity or the host is hot: the
// scaling report then speaks for it.
func (h hostState) active() bool { return h.hot || h.backedOff > 0 }

// hostFloorLocked is the least the back-off lowers the capacity to:
// a range's min, never below 1; a fixed node's 1. l.mu is held.
func (l *Loop) hostFloorLocked() int {
	if r := l.rangeLocked(); r.IsRange() {
		return max(r.Min, 1)
	}
	return 1
}

// planLocked is the plan figure: the capacity before the back-off, as
// Capacity was before it existed. l.mu is held.
func (l *Loop) planLocked() int {
	if l.capacity > 0 || l.scaling.scaler != nil {
		return l.capacity
	}
	if r := l.rangeLocked(); r.IsRange() {
		return r.Min
	}
	return l.rangeLocked().Max
}

// inForceLocked is plan less the slots held back, never below the floor
// (and a plan already below the floor as it is). l.mu is held.
func (l *Loop) inForceLocked(plan int) int {
	return plan - l.heldBackLocked(plan)
}

// heldBackLocked is how many of plan's slots the back-off holds now:
// backedOff, at most down to the floor. l.mu is held.
func (l *Loop) heldBackLocked(plan int) int {
	return min(l.backoff.backedOff, max(plan-l.hostFloorLocked(), 0))
}

// hostHot says the host was hot at the last tick: claim nothing new.
func (l *Loop) hostHot() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.backoff.hot
}

// hostBackoff reads the machine and moves the back-off one step: one
// slot down per HostStepEvery while hot, one up per HostRecoverEvery of
// quiet. No Machine
// (a non-Linux host) or a failed read changes nothing.
func (l *Loop) hostBackoff() {
	if l.Machine == nil {
		return
	}
	m, err := l.Machine.Read()
	if err != nil {
		return
	}
	thresholds := l.HostThresholds()
	now := l.now()

	l.mu.Lock()
	st := &l.backoff
	hot, why := m.Hot(thresholds, st.hot)
	sayTempAbsent := !m.HasTemp && !st.tempAbsentSaid
	if sayTempAbsent {
		st.tempAbsentSaid = true
	}
	st.reading, st.read, st.hot = m, true, hot
	plan := l.planLocked()
	// what was held back is clamped to what the plan now leaves
	st.backedOff = l.heldBackLocked(plan)
	was := plan - st.backedOff
	var line string
	if hot {
		st.reason, st.quietSince = why, time.Time{}
		if was > l.hostFloorLocked() && (st.steppedAt.IsZero() || now.Sub(st.steppedAt) >= HostStepEvery) {
			st.backedOff++
			st.steppedAt = now
			line = fmt.Sprintf("capacity %d → %d: %s", was, was-1, why)
		}
	} else if st.backedOff > 0 {
		if st.quietSince.IsZero() {
			st.quietSince = now
		}
		if quiet := now.Sub(st.quietSince); quiet >= HostRecoverEvery {
			st.backedOff--
			st.quietSince = now
			line = fmt.Sprintf("capacity %d → %d: host quiet %s", was, was+1, quietFigure(quiet))
		}
	} else {
		st.quietSince = time.Time{}
	}
	if st.backedOff == 0 && !hot {
		st.reason = ""
	}
	l.mu.Unlock()
	if sayTempAbsent {
		l.logf("host: no package temperature zone; judging by load alone")
	}
	if line != "" {
		l.logf("%s", line)
	}
}

// quietFigure is a quiet spell in whole minutes, as `5m`.
func quietFigure(d time.Duration) string {
	return fmt.Sprintf("%dm", int(d/time.Minute))
}
