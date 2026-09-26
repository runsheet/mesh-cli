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
// the CPU count and the memory available, in bytes.
type MachineReading struct {
	Load1        float64
	CPUs         int
	MemAvailable uint64
}

// Busy says the machine can take no more sessions: load at or above the
// CPU count, or free memory at or below MinFreeMemory.
func (m MachineReading) Busy() bool {
	return m.Load1 >= float64(m.CPUs) || m.MemAvailable <= MinFreeMemory
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
	return ProcMachine{Root: "/"}
}

// ProcMachine reads /proc/loadavg and /proc/meminfo under Root.
type ProcMachine struct{ Root string }

func (p ProcMachine) Read() (MachineReading, error) {
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
			return m, nil
		}
	}
	return m, errors.New("/proc/meminfo has no MemAvailable")
}
