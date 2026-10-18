package node

import (
	"fmt"
)

// The memory floor (ADR-037; dl360, 2026-10-18, when 208 validation
// steps in one day were killed for memory, exit 137): when node.yaml sets
// host.min_free_memory, every tick, before any claim, the loop reads the
// machine's memory available and judges it by that floor
// (HostConfig.MemoryFloor). At or under the floor it claims nothing of
// any kind — build, evaluation, rebase, plan, research, merge batch or
// main validation — as the disk floor does (diskfloor.go, ADR-035), and
// says so once as it
// enters the state, `memory_low: …`, and once as it leaves it, `memory
// ok: …`. Running sessions are never touched. A node with no machine
// probe (off Linux) is not judged, and neither is one that leaves the key
// out: it keeps ADR-004's cap alone, growth held at the sessions it runs
// under MinFreeMemory, and a fixed capacity unaffected by memory.

// memState is the floor as last judged: whether free memory was at or
// under it, and the Finding that says so while it is.
type memState struct {
	low     bool
	finding *Finding
	// readErr is the last read error said, so a failing read is logged
	// once until it changes.
	readErr string
}

// memLow says free memory was at or under the floor at the last tick:
// claim nothing.
func (l *Loop) memLow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.memState.low
}

// MemoryFindings is the floor's Finding while the node is under it —
// kind memory_low, critical, its bytes the memory available — else
// none: the quartermaster's shape, as DiskFindings.
func (l *Loop) MemoryFindings() []Finding {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.memState.finding == nil {
		return nil
	}
	return []Finding{*l.memState.finding}
}

// memFloor reads the machine and judges its free memory by the floor in
// force, logging a crossing either way once. A failed read keeps the
// last judgement and is said once per distinct error.
func (l *Loop) memFloor() {
	h := l.HostThresholds()
	if l.Machine == nil || h.MinFreeMemory == "" {
		l.mu.Lock()
		was := l.memState.low
		l.memState = memState{}
		l.mu.Unlock()
		if was {
			l.logf("memory ok: host.min_free_memory is not set; claiming again")
		}
		return
	}
	m, err := l.Machine.Read()
	l.mu.Lock()
	ms := &l.memState
	if err != nil {
		say := err.Error() != ms.readErr
		ms.readErr = err.Error()
		l.mu.Unlock()
		if say {
			l.logf("memory: cannot read the machine: %v; keeping the last judgement", err)
		}
		return
	}
	ms.readErr = ""
	floor := h.MemoryFloor(m.MemTotal)
	low := m.ShortOfMemoryAt(h)
	was := ms.low
	ms.low = low
	var line string
	if low {
		line = fmt.Sprintf("memory_low: %s free <= floor %s; claiming nothing", gib(m.MemAvailable), gib(floor))
		ms.finding = &Finding{Kind: "memory_low", Severity: "critical", Subject: "memory", Detail: line, Bytes: int64(m.MemAvailable), At: l.now()}
	} else {
		ms.finding = nil
		line = fmt.Sprintf("memory ok: %s free > floor %s; claiming again", gib(m.MemAvailable), gib(floor))
	}
	l.mu.Unlock()
	if low != was {
		l.logf("%s", line)
	}
}
