package nodev1

import "fmt"

// Host is the floor past which the node backs off a hot or
// short-of-memory host (Mesh v1 spec amendment, "A node backs off a hot
// host", reimplemented from the legacy node's HostConfig since nodev1
// may not import internal/node; guard_test.go). Each field left zero,
// the default, never gates on that reading.
type Host struct {
	// LoadPerCoreMax is the load average per core past which the host is
	// read as hot.
	LoadPerCoreMax float64
	// MinFreeMemory is the bytes of free memory at or under which the
	// host is read as short of memory.
	MinFreeMemory uint64
	// PackageTempMaxC is the package thermal zone's reading, in degrees
	// Celsius, past which the host is read as hot. Checked only when the
	// reading's HasTemp is true; a probe that found no thermal zone
	// never gates on it.
	PackageTempMaxC float64
}

// MachineReading is one read of the host's load and memory.
type MachineReading struct {
	// Load1 is the 1-minute load average; PerCore divides it by CPUs.
	Load1 float64
	CPUs  int
	// MemAvailable is the kernel's own estimate of memory a new process
	// could use without swapping (/proc/meminfo's MemAvailable).
	MemAvailable uint64

	// CPUSomeAvg10 and CPUSomeAvg60 are /proc/pressure/cpu's `some`
	// line's avg10 and avg60: the share of the last 10 and 60 seconds
	// some task spent stalled on CPU, as a percentage. Meaningful only
	// when HasCPUPressure.
	CPUSomeAvg10, CPUSomeAvg60 float64
	// HasCPUPressure says /proc/pressure/cpu was found and read. False
	// off Linux, or on a kernel built without PSI (no
	// /proc/pressure/cpu) — not an error.
	HasCPUPressure bool

	// MemSomeAvg10 and MemSomeAvg60 are /proc/pressure/memory's `some`
	// line's avg10 and avg60, the same shape as the CPU pressure above.
	// Meaningful only when HasMemPressure.
	MemSomeAvg10, MemSomeAvg60 float64
	// HasMemPressure says /proc/pressure/memory was found and read.
	// False off Linux, or on a kernel built without PSI — not an error.
	HasMemPressure bool

	// PackageTempC is the package thermal zone's reading in degrees
	// Celsius: the max of every zone typed x86_pkg_temp or TCPU, a
	// multi-socket host having one zone per package. Meaningful only
	// when HasTemp.
	PackageTempC float64
	// HasTemp says at least one package thermal zone was found and
	// read. False off Linux, with no /sys/class/thermal, or with no
	// zone of either type — none of that is an error.
	HasTemp bool

	// LeaseDisk and CacheDisk are the free space and percent used at
	// the lease path (where each lease's own job directory lives) and
	// the cache path (the build cache), as HostProbe last read them.
	LeaseDisk, CacheDisk DiskReading
}

// HostProbe reads the host's full MachineReading once — load, memory,
// CPU and memory pressure, package temperature, and the disk reading
// at two paths, leasePath and cachePath — independent of hostGate's
// simpler MachineProbe above. A failed read of one source never fails
// the whole reading: that field comes back zero, its own Has flag
// false, never an error (hostprobe_linux.go, hostprobe_other.go). The
// runtime's once-a-minute monitor (WatchHost, runtime.go) is its one
// caller.
type HostProbe interface {
	Read(leasePath, cachePath string) (MachineReading, error)
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
// not: Machine reads past Host.LoadPerCoreMax load per core, at or
// under Host.MinFreeMemory free, or (with a reading that has one) past
// Host.PackageTempMaxC degrees. Checked once per pass through Run's
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
	if max := r.Host.PackageTempMaxC; max > 0 && reading.HasTemp && reading.PackageTempC > max {
		return fmt.Sprintf("host hot: package %.1f°C over %.1f°C", reading.PackageTempC, max)
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
