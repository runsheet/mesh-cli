package node

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// MinFreeMemory is the free memory below which the machine is busy: CI
// runs on the same mini, and a build that swaps is a build that fails.
const MinFreeMemory = 2 << 30

// MachineReading is the host as last read: the 1-minute load average,
// the CPU count, the memory available in bytes, and the package
// thermal zone's temperature when one was found.
type MachineReading struct {
	Load1        float64
	CPUs         int
	MemAvailable uint64
	// PackageTempC is the package thermal zone's reading in degrees
	// Celsius; meaningful only when HasTemp.
	PackageTempC float64
	// HasTemp says a package thermal zone (x86_pkg_temp or TCPU) was
	// found and read. False off Linux, with no /sys/class/thermal, or
	// with no zone of either type — none of that is an error.
	HasTemp bool
}

// BusyAt says the machine is at or past h's load threshold — the
// 1-minute load average at or above h.LoadPerCoreMax per core — or free
// memory at or below MinFreeMemory. The one notion of "busy": Busy and
// Hot both go through it, each at their own threshold.
func (m MachineReading) BusyAt(h HostConfig) bool {
	return (m.CPUs > 0 && m.Load1/float64(m.CPUs) >= h.LoadPerCoreMax) || m.MemAvailable <= MinFreeMemory
}

// ShortOfMemory says free memory is at or below MinFreeMemory: the
// scalers' growth cap (ReasonLowMemory).
func (m MachineReading) ShortOfMemory() bool {
	return m.MemAvailable <= MinFreeMemory
}

// Busy says the machine can take no more sessions: load at or above one
// per core (ADR-004's own threshold, unrelated to node.yaml's host:
// load_per_core_max, which backs the scaler off earlier, at its own
// usually lower bar), or free memory at or below MinFreeMemory.
func (m MachineReading) Busy() bool {
	return m.BusyAt(HostConfig{LoadPerCoreMax: 1})
}

// Hot says the machine is past h's thresholds, and why: load over
// h.LoadPerCoreMax per core wins when both load and temperature are
// past theirs, since it is read every tick and never absent the way a
// thermal zone can be. Quiet (false) carries no reason.
func (m MachineReading) Hot(h HostConfig) (bool, Reason) {
	if m.CPUs > 0 && m.Load1/float64(m.CPUs) > h.LoadPerCoreMax {
		return true, Reason(fmt.Sprintf("host hot: load %s/%d cores", loadFigure(m.Load1), m.CPUs))
	}
	if m.HasTemp && m.PackageTempC > h.PackageTempMaxC {
		return true, Reason(fmt.Sprintf("host hot: %.0f°C", m.PackageTempC))
	}
	return false, ""
}

// loadFigure is how a 1-minute load average is shown in a host hot
// reason: whole at 10 or more, one decimal place below it.
func loadFigure(load1 float64) string {
	if load1 >= 10 {
		return fmt.Sprintf("%.0f", load1)
	}
	return fmt.Sprintf("%.1f", load1)
}

// MachineProbe reads the host; an error sets no cap.
type MachineProbe interface {
	Read() (MachineReading, error)
}

// HostMachine is this machine's probe: /proc on Linux; elsewhere nil,
// which imposes no cap.
func HostMachine() MachineProbe {
	if runtime.GOOS != "linux" {
		return nil
	}
	return &ProcMachine{Root: "/"}
}

// ProcMachine reads /proc/loadavg, /proc/meminfo and the package
// thermal zone under Root.
type ProcMachine struct {
	Root string
	// TempAbsentLogged is set the first time Read finds no package
	// thermal zone (no /sys/class/thermal, or none of its zones typed
	// x86_pkg_temp or TCPU) — not an error, so a caller holding the
	// probe checks it before logging `host: no package temperature
	// zone`, and says it once rather than every tick.
	TempAbsentLogged bool
}

func (p *ProcMachine) Read() (MachineReading, error) {
	m := MachineReading{CPUs: runtime.NumCPU()}
	load, err := os.ReadFile(filepath.Join(p.Root, "proc", "loadavg"))
	if err != nil {
		return m, err
	}
	f := strings.Fields(string(load))
	if len(f) == 0 {
		return m, errors.New("/proc/loadavg is empty")
	}
	if m.Load1, err = strconv.ParseFloat(f[0], 64); err != nil {
		return m, fmt.Errorf("/proc/loadavg: %w", err)
	}
	mem, err := os.ReadFile(filepath.Join(p.Root, "proc", "meminfo"))
	if err != nil {
		return m, err
	}
	sc := bufio.NewScanner(bytes.NewReader(mem))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && f[0] == "MemAvailable:" {
			kb, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				return m, fmt.Errorf("/proc/meminfo: %w", err)
			}
			m.MemAvailable = kb * 1024
			m.PackageTempC, m.HasTemp = p.readPackageTemp()
			return m, nil
		}
	}
	return m, errors.New("/proc/meminfo has no MemAvailable")
}

// readPackageTemp scans Root's thermal zones for the one typed
// x86_pkg_temp or TCPU and reads its temperature, converted from the
// millidegrees its temp file holds to degrees Celsius. No
// /sys/class/thermal, or no zone of either type, is not an error: it
// sets TempAbsentLogged (unless already set) and answers no reading.
func (p *ProcMachine) readPackageTemp() (float64, bool) {
	zones, err := filepath.Glob(filepath.Join(p.Root, "sys", "class", "thermal", "thermal_zone*"))
	if err != nil || len(zones) == 0 {
		p.TempAbsentLogged = true
		return 0, false
	}
	for _, zone := range zones {
		typ, err := os.ReadFile(filepath.Join(zone, "type"))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(typ)) {
		case "x86_pkg_temp", "TCPU":
		default:
			continue
		}
		raw, err := os.ReadFile(filepath.Join(zone, "temp"))
		if err != nil {
			continue
		}
		milli, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
		if err != nil {
			continue
		}
		return milli / 1000, true
	}
	p.TempAbsentLogged = true
	return 0, false
}
