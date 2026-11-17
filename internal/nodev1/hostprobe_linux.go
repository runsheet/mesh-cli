//go:build linux

package nodev1

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
	"syscall"
)

// ProcHostProbe is this machine's HostProbe: /proc's loadavg, meminfo
// and pressure files, and /sys/class/thermal's package zones, all
// under Root — the mini runs linux/amd64; Root lets a test feed a
// fixture tree in place of the real /proc and /sys. Read's leasePath
// and cachePath are read with statfs directly, never under Root,
// since they name real directories rather than a fixture.
type ProcHostProbe struct {
	Root string
}

// Read is HostProbe's: /proc/loadavg and /proc/meminfo's MemAvailable
// must read, or the whole call errors, as the legacy node's probe
// always has; every other field — CPU and memory pressure, package
// temperature, and the two disk readings — is read independently, and
// a source that is missing or unreadable leaves its field zero and
// its own Has flag false, never an error.
func (p *ProcHostProbe) Read(leasePath, cachePath string) (MachineReading, error) {
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
	found := false
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && f[0] == "MemAvailable:" {
			kb, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				return m, fmt.Errorf("/proc/meminfo: %w", err)
			}
			m.MemAvailable = kb * 1024
			found = true
			break
		}
	}
	if !found {
		return m, errors.New("/proc/meminfo has no MemAvailable")
	}
	m.CPUSomeAvg10, m.CPUSomeAvg60, m.HasCPUPressure = parsePressureFile(filepath.Join(p.Root, "proc", "pressure", "cpu"))
	m.MemSomeAvg10, m.MemSomeAvg60, m.HasMemPressure = parsePressureFile(filepath.Join(p.Root, "proc", "pressure", "memory"))
	m.PackageTempC, m.HasTemp = p.readPackageTemp()
	m.LeaseDisk = diskReadingAt(leasePath)
	m.CacheDisk = diskReadingAt(cachePath)
	return m, nil
}

// readPackageTemp scans Root's thermal zones for every zone typed
// x86_pkg_temp or TCPU and reads the highest of them, converted from
// the millidegrees its temp file holds to degrees Celsius — a
// multi-socket host has one zone per package, and the hottest one is
// the one worth backing off for. No /sys/class/thermal, or no zone of
// either type, is not an error: it answers no reading.
func (p *ProcHostProbe) readPackageTemp() (float64, bool) {
	zones, err := filepath.Glob(filepath.Join(p.Root, "sys", "class", "thermal", "thermal_zone*"))
	if err != nil || len(zones) == 0 {
		return 0, false
	}
	var max float64
	found := false
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
		c := milli / 1000
		if !found || c > max {
			max, found = c, true
		}
	}
	return max, found
}

// parsePressureFile reads a PSI pressure file (/proc/pressure/cpu or
// /proc/pressure/memory) and parses its `some` line's avg10 and
// avg60: the share of the last 10 and 60 seconds some task spent
// stalled. A missing file, or no `some` line, is not an error: it
// answers no reading.
func parsePressureFile(path string) (avg10, avg60 float64, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || f[0] != "some" {
			continue
		}
		var a10, a60 float64
		var have10, have60 bool
		for _, kv := range f[1:] {
			k, v, found := strings.Cut(kv, "=")
			if !found {
				continue
			}
			switch k {
			case "avg10":
				if a10, err = strconv.ParseFloat(v, 64); err == nil {
					have10 = true
				}
			case "avg60":
				if a60, err = strconv.ParseFloat(v, 64); err == nil {
					have60 = true
				}
			}
		}
		if have10 && have60 {
			return a10, a60, true
		}
		return 0, 0, false
	}
	return 0, 0, false
}

// diskReadingAt reads path's filesystem free space and percent used
// with statfs. "" or an unreadable path answers DiskReading{}, Has
// false — never an error.
func diskReadingAt(path string) DiskReading {
	if path == "" {
		return DiskReading{}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return DiskReading{}
	}
	free := st.Bavail * uint64(st.Bsize)
	total := st.Blocks * uint64(st.Bsize)
	d := DiskReading{FreeBytes: free, Has: true}
	if total > 0 {
		d.PercentUsed = float64(total-free) / float64(total) * 100
	}
	return d
}
