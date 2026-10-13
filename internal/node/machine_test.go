package node

import (
	"fmt"
	"os"
	"testing"
)

// ProcMachine reads the package thermal zone under Root: the zone
// typed x86_pkg_temp, with its temp file in millidegrees Celsius.
func TestProcMachineReadsX86PkgTemp(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/proc/loadavg", "1.00 1.00 1.00 1/100 1\n")
	writeFile(t, root+"/proc/meminfo", "MemAvailable:    8388608 kB\n")
	writeFile(t, root+"/sys/class/thermal/thermal_zone0/type", "x86_pkg_temp\n")
	writeFile(t, root+"/sys/class/thermal/thermal_zone0/temp", "101000\n")

	pm := ProcMachine{Root: root}
	m, err := pm.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasTemp || m.PackageTempC != 101 {
		t.Fatalf("read %+v, want PackageTempC 101, HasTemp true", m)
	}
	if pm.TempAbsentLogged {
		t.Fatal("a zone was found; TempAbsentLogged must stay false")
	}
}

// The zone may instead be typed TCPU (Apple Silicon and some ARM
// boards use this name).
func TestProcMachineReadsTCPU(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/proc/loadavg", "1.00 1.00 1.00 1/100 1\n")
	writeFile(t, root+"/proc/meminfo", "MemAvailable:    8388608 kB\n")
	writeFile(t, root+"/sys/class/thermal/thermal_zone1/type", "TCPU\n")
	writeFile(t, root+"/sys/class/thermal/thermal_zone1/temp", "101000\n")

	m, err := (&ProcMachine{Root: root}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasTemp || m.PackageTempC != 101 {
		t.Fatalf("read %+v, want PackageTempC 101, HasTemp true", m)
	}
}

// No /sys/class/thermal at all is not an error: HasTemp is false, and
// the probe flags it so a caller holding it logs the absence once.
func TestProcMachineNoThermalDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/proc/loadavg", "1.00 1.00 1.00 1/100 1\n")
	writeFile(t, root+"/proc/meminfo", "MemAvailable:    8388608 kB\n")

	pm := ProcMachine{Root: root}
	m, err := pm.Read()
	if err != nil {
		t.Fatal(err)
	}
	if m.HasTemp {
		t.Fatalf("read %+v, want HasTemp false with no thermal dir", m)
	}
	if !pm.TempAbsentLogged {
		t.Fatal("no thermal dir: TempAbsentLogged must be set")
	}
}

// loadavg and meminfo alone — no thermal zone of either known type, and
// no zone matching one planted beside irrelevant ones — still read the
// load and memory, HasTemp false.
func TestProcMachineLoadavgOnly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/proc/loadavg", "3.47 2.10 1.90 2/812 12345\n")
	writeFile(t, root+"/proc/meminfo", "MemTotal:       16318548 kB\nMemAvailable:    1048576 kB\n")
	writeFile(t, root+"/sys/class/thermal/thermal_zone0/type", "iwlwifi_1\n")
	writeFile(t, root+"/sys/class/thermal/thermal_zone0/temp", "42000\n")

	pm := ProcMachine{Root: root}
	m, err := pm.Read()
	if err != nil {
		t.Fatal(err)
	}
	if m.Load1 != 3.47 || m.MemAvailable != 1<<30 || m.CPUs < 1 {
		t.Fatalf("read %+v", m)
	}
	if m.HasTemp {
		t.Fatalf("read %+v, want HasTemp false: no zone typed x86_pkg_temp or TCPU", m)
	}
	if !pm.TempAbsentLogged {
		t.Fatal("no matching zone: TempAbsentLogged must be set")
	}
}

// ProcMachine.Read parses /proc/pressure/cpu's `some` line's avg10 and
// avg60, and PerCore gives the load average per core.
func TestProcMachineReadsCPUPressure(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/proc/loadavg", "3.60 3.00 2.50 2/812 12345\n")
	writeFile(t, root+"/proc/meminfo", "MemAvailable:    8388608 kB\n")
	writeFile(t, root+"/proc/pressure/cpu", "some avg10=12.34 avg60=45.67 avg300=10.00 total=98765\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")

	m, err := (&ProcMachine{Root: root}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasCPUPressure || m.CPUSomeAvg10 != 12.34 || m.CPUSomeAvg60 != 45.67 {
		t.Fatalf("read %+v, want some avg10 12.34, avg60 45.67", m)
	}
	if got := m.PerCore(); got != m.Load1/float64(m.CPUs) {
		t.Fatalf("PerCore() = %v, want Load1/CPUs = %v", got, m.Load1/float64(m.CPUs))
	}
}

// No /proc/pressure/cpu at all (a kernel without PSI) is not an error:
// HasCPUPressure is false, and the rest of the reading still comes
// through.
func TestProcMachineNoCPUPressure(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/proc/loadavg", "1.00 1.00 1.00 1/100 1\n")
	writeFile(t, root+"/proc/meminfo", "MemAvailable:    8388608 kB\n")

	m, err := (&ProcMachine{Root: root}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if m.HasCPUPressure || m.CPUSomeAvg10 != 0 || m.CPUSomeAvg60 != 0 {
		t.Fatalf("read %+v, want no CPU pressure", m)
	}
	if m.Load1 != 1 || m.MemAvailable != 8388608*1024 {
		t.Fatalf("read %+v, want the load and memory still read", m)
	}
}

// ProcMachine.Read also parses the node user's own cgroup v2 slice's
// cpu.pressure under user.slice/user-<uid>.slice, distinct from the
// host-wide /proc/pressure/cpu.
func TestProcMachineReadsSliceCPUPressure(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/proc/loadavg", "3.60 3.00 2.50 2/812 12345\n")
	writeFile(t, root+"/proc/meminfo", "MemAvailable:    8388608 kB\n")
	slicePath := fmt.Sprintf("%s/sys/fs/cgroup/user.slice/user-%d.slice/cpu.pressure", root, os.Getuid())
	writeFile(t, slicePath, "some avg10=12.50 avg60=30.00 avg300=5.00 total=4321\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")

	m, err := (&ProcMachine{Root: root}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasSliceCPUPressure || m.SliceCPUSomeAvg10 != 12.5 || m.SliceCPUSomeAvg60 != 30 {
		t.Fatalf("read %+v, want slice some avg10 12.5, avg60 30, HasSliceCPUPressure true", m)
	}
}

// No user.slice/user-<uid>.slice/cpu.pressure (cgroup v1, or off
// Linux) is not an error: HasSliceCPUPressure is false, and the rest
// of the reading still comes through.
func TestProcMachineNoSliceCPUPressure(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/proc/loadavg", "1.00 1.00 1.00 1/100 1\n")
	writeFile(t, root+"/proc/meminfo", "MemAvailable:    8388608 kB\n")

	m, err := (&ProcMachine{Root: root}).Read()
	if err != nil {
		t.Fatal(err)
	}
	if m.HasSliceCPUPressure || m.SliceCPUSomeAvg10 != 0 || m.SliceCPUSomeAvg60 != 0 {
		t.Fatalf("read %+v, want no slice CPU pressure", m)
	}
	if m.Load1 != 1 || m.MemAvailable != 8388608*1024 {
		t.Fatalf("read %+v, want the load and memory still read", m)
	}
}

// Overloaded is true, with a reason naming load per core, the raw
// load, the cores and the CPU pressure, once load per core passes
// host.overload_per_core; false below it.
func TestMachineReadingOverloaded(t *testing.T) {
	h := HostConfig{OverloadPerCore: 1.0}
	over := MachineReading{Load1: 138.24, CPUs: 96, CPUSomeAvg60: 71, HasCPUPressure: true}
	hot, reason := over.Overloaded(h)
	if !hot {
		t.Fatalf("Overloaded() = false, want true for load/core %v", over.PerCore())
	}
	if want := "host overloaded: load/core 1.44 (138.2 on 96 cores), cpu some avg60 71%"; string(reason) != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}

	under := MachineReading{Load1: 72, CPUs: 96}
	hot, reason = under.Overloaded(h)
	if hot || reason != "" {
		t.Fatalf("Overloaded() = (%v, %q), want (false, \"\") for load/core %v", hot, reason, under.PerCore())
	}
}

// Hot judges load first — "host hot: load 55/18 cores", the average
// formatted whole at 10 or more — then, only when load is under its
// threshold, the package temperature; load wins when both are past
// theirs. Quiet carries no reason.
func TestMachineReadingHot(t *testing.T) {
	defaults := HostConfig{}.Effective()

	for _, tc := range []struct {
		name   string
		m      MachineReading
		h      HostConfig
		want   bool
		reason Reason
	}{
		{"load over threshold", MachineReading{Load1: 55, CPUs: 18}, defaults, true, "host hot: load 55/18 cores"},
		{"temp over threshold", MachineReading{Load1: 9, CPUs: 18, PackageTempC: 101, HasTemp: true}, defaults, true, "host hot: 101°C"},
		{"quiet", MachineReading{Load1: 9, CPUs: 18, PackageTempC: 70, HasTemp: true}, defaults, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hot, reason := tc.m.Hot(tc.h)
			if hot != tc.want || reason != tc.reason {
				t.Fatalf("Hot() = (%v, %q), want (%v, %q)", hot, reason, tc.want, tc.reason)
			}
		})
	}
}
