package nodev1

import "fmt"

// Host is the floor past which the node backs off a hot or
// short-of-memory host (Mesh v1 spec amendment, "A node backs off a hot
// host", reimplemented from the legacy node's HostConfig since nodev1
// may not import internal/node; guard_test.go). Either field left zero,
// the default, never gates on that reading.
type Host struct {
	// LoadPerCoreMax is the load average per core past which the host is
	// read as hot.
	LoadPerCoreMax float64
	// MinFreeMemory is the bytes of free memory at or under which the
	// host is read as short of memory.
	MinFreeMemory uint64
}

// MachineReading is one read of the host's load and memory.
type MachineReading struct {
	// Load1 is the 1-minute load average; PerCore divides it by CPUs.
	Load1 float64
	CPUs  int
	// MemAvailable is the kernel's own estimate of memory a new process
	// could use without swapping (/proc/meminfo's MemAvailable).
	MemAvailable uint64
}

// PerCore is Load1 divided by CPUs; 0 when CPUs is 0, so a reading with
// no core count never gates on load.
func (m MachineReading) PerCore() float64 {
	if m.CPUs <= 0 {
		return 0
	}
	return m.Load1 / float64(m.CPUs)
}

// MachineProbe reads the host's current load and memory. A failed read
// never gates: the node keeps its last judgement rather than guess from
// an error.
type MachineProbe interface {
	Read() (MachineReading, error)
}

// hostGate is "" when the node may take a new lease, else why it may
// not: Machine reads past Host.LoadPerCoreMax load per core, or at or
// under Host.MinFreeMemory free. Checked once per pass through Run's
// take loop, before r.API.Take — never against a lease already running,
// which is left to finish whatever the host does meanwhile. Nil Machine
// never gates.
func (r *Runtime) hostGate() string {
	if r.Machine == nil {
		return ""
	}
	reading, err := r.Machine.Read()
	if err != nil {
		return ""
	}
	if max := r.Host.LoadPerCoreMax; max > 0 {
		if per := reading.PerCore(); per > max {
			return fmt.Sprintf("host hot: load/core %.2f over %.2f", per, max)
		}
	}
	if floor := r.Host.MinFreeMemory; floor > 0 && reading.MemAvailable <= floor {
		return fmt.Sprintf("host short of memory: %d bytes free, floor %d", reading.MemAvailable, floor)
	}
	return ""
}

// noteHostGate logs a crossing only — entering a reason, or leaving one
// ("") — never the same state twice running.
func (r *Runtime) noteHostGate(reason string) {
	r.mu.Lock()
	changed := reason != r.hostGateReason
	r.hostGateReason = reason
	r.mu.Unlock()
	if !changed {
		return
	}
	if reason == "" {
		r.logf("nodev1: host ok; taking leases again")
		return
	}
	r.logf("nodev1: %s; taking no new lease", reason)
}
