package node

import (
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
