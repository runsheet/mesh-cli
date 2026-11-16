//go:build linux

package nodev1

import (
	"os"
	"path/filepath"
	"testing"
)

// hostFixture makes a Root-rooted /proc and /sys tree: loadavg,
// meminfo, both pressure files and two x86_pkg_temp thermal zones (one
// hotter, to show the reading is their max).
func hostFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "proc", "loadavg"), "2.50 1.80 1.20 3/456 789\n")
	mustWrite(t, filepath.Join(root, "proc", "meminfo"), "MemTotal:       16000000 kB\nMemAvailable:    4000000 kB\n")
	mustWrite(t, filepath.Join(root, "proc", "pressure", "cpu"), "some avg10=1.50 avg60=2.50 avg300=1.00 total=123\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	mustWrite(t, filepath.Join(root, "proc", "pressure", "memory"), "some avg10=3.50 avg60=4.50 avg300=2.00 total=456\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	mustWrite(t, filepath.Join(root, "sys", "class", "thermal", "thermal_zone0", "type"), "x86_pkg_temp\n")
	mustWrite(t, filepath.Join(root, "sys", "class", "thermal", "thermal_zone0", "temp"), "55000\n")
	mustWrite(t, filepath.Join(root, "sys", "class", "thermal", "thermal_zone1", "type"), "x86_pkg_temp\n")
	mustWrite(t, filepath.Join(root, "sys", "class", "thermal", "thermal_zone1", "temp"), "71000\n")
	return root
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The probe's reading carries load, both pressures, memory, both disk
// paths' free space and percent used, and the package temperature as
// the max of the fixture's two x86_pkg_temp zones — 71, not 55.
func TestMachineReadingCarriesPressureDiskAndTemp(t *testing.T) {
	root := hostFixture(t)
	leaseDir, cacheDir := t.TempDir(), t.TempDir()
	probe := &ProcHostProbe{Root: root}

	m, err := probe.Read(leaseDir, cacheDir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if m.Load1 != 2.5 {
		t.Fatalf("Load1 = %v, want 2.5", m.Load1)
	}
	if m.CPUs <= 0 {
		t.Fatalf("CPUs = %d, want > 0", m.CPUs)
	}
	if m.MemAvailable != 4000000*1024 {
		t.Fatalf("MemAvailable = %d, want %d", m.MemAvailable, 4000000*1024)
	}
	if !m.HasCPUPressure || m.CPUSomeAvg10 != 1.5 || m.CPUSomeAvg60 != 2.5 {
		t.Fatalf("CPU pressure: has %v avg10 %v avg60 %v", m.HasCPUPressure, m.CPUSomeAvg10, m.CPUSomeAvg60)
	}
	if !m.HasMemPressure || m.MemSomeAvg10 != 3.5 || m.MemSomeAvg60 != 4.5 {
		t.Fatalf("memory pressure: has %v avg10 %v avg60 %v", m.HasMemPressure, m.MemSomeAvg10, m.MemSomeAvg60)
	}
	if !m.HasTemp || m.PackageTempC != 71 {
		t.Fatalf("package temp: has %v temp %v, want 71 (the hotter of the two zones)", m.HasTemp, m.PackageTempC)
	}
	if !m.LeaseDisk.Has || m.LeaseDisk.FreeBytes == 0 {
		t.Fatalf("lease disk: %+v, want Has with free bytes", m.LeaseDisk)
	}
	if !m.CacheDisk.Has || m.CacheDisk.FreeBytes == 0 {
		t.Fatalf("cache disk: %+v, want Has with free bytes", m.CacheDisk)
	}
	if m.LeaseDisk.PercentUsed < 0 || m.LeaseDisk.PercentUsed > 100 {
		t.Fatalf("lease disk percent used: %v, want 0-100", m.LeaseDisk.PercentUsed)
	}
}

// A fixture missing /proc/pressure/memory and a thermal zone answers
// those fields zero, Has false, never an error — the rest of the
// reading still comes through.
func TestMachineReadingMissingSourcesAreZeroNotError(t *testing.T) {
	root := hostFixture(t)
	if err := os.RemoveAll(filepath.Join(root, "proc", "pressure", "memory")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "sys", "class", "thermal")); err != nil {
		t.Fatal(err)
	}
	probe := &ProcHostProbe{Root: root}

	m, err := probe.Read(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.HasMemPressure || m.MemSomeAvg10 != 0 || m.MemSomeAvg60 != 0 {
		t.Fatalf("memory pressure with no source file: %+v, want zero and false", m)
	}
	if m.HasTemp || m.PackageTempC != 0 {
		t.Fatalf("package temp with no thermal zone: has %v temp %v, want false and 0", m.HasTemp, m.PackageTempC)
	}
	if !m.HasCPUPressure {
		t.Fatalf("CPU pressure should still read with its own file present")
	}

	// a disk path that cannot be statfs'd is the same: zero, Has false
	m2, err := probe.Read(filepath.Join(root, "no-such-dir"), filepath.Join(root, "also-missing"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m2.LeaseDisk.Has || m2.LeaseDisk != (DiskReading{}) {
		t.Fatalf("lease disk at a missing path: %+v, want the zero value", m2.LeaseDisk)
	}
	if m2.CacheDisk.Has || m2.CacheDisk != (DiskReading{}) {
		t.Fatalf("cache disk at a missing path: %+v, want the zero value", m2.CacheDisk)
	}
}
