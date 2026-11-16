package nodev1

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// fakeHostProbe is a HostProbe with a reading fixed in advance, for a
// test that wants WatchHost's first (synchronous, pre-loop) apply to
// land a known value rather than ProcHostProbe's real one.
type fakeHostProbe struct{ reading MachineReading }

func (f fakeHostProbe) Read(leasePath, cachePath string) (MachineReading, error) {
	return f.reading, nil
}

// The heartbeat carries the host's latest cached reading (Mesh v1 spec
// amendment, "A node reports its host's cpu, memory, disk and
// temperature"): WatchHost's own cache (hostReading/hostRead), not a
// fresh read of its own.
func TestHeartbeatCarriesTheHostReading(t *testing.T) {
	reading := MachineReading{
		Load1: 2.5, CPUs: 4, MemAvailable: 2 << 30,
		CPUSomeAvg10: 10, CPUSomeAvg60: 12, HasCPUPressure: true,
		MemSomeAvg10: 1, MemSomeAvg60: 2, HasMemPressure: true,
		PackageTempC: 61.5, HasTemp: true,
		LeaseDisk: DiskReading{FreeBytes: 40 << 30, PercentUsed: 60, Has: true},
		CacheDisk: DiskReading{FreeBytes: 30 << 30, PercentUsed: 70, Has: true},
	}
	var hb meshapi.HeartbeatRequest
	sess := &stubSession{calls: []string{"conform/brief", "conform/submit"}, result: SessionResult{SessionID: "s"}}
	var g *rig
	sess.inspect = func(spec SessionSpec) {
		g.rt.heartbeat(context.Background())
		calls := g.fake.Calls("heartbeat")
		if err := json.Unmarshal(calls[len(calls)-1].Body, &hb); err != nil {
			t.Fatal(err)
		}
	}
	g = newRig(t, sess)
	g.rt.HostProbe = fakeHostProbe{reading: reading}
	g.offer(t, nil)
	if err := run(t, g, context.Background()); err != nil {
		t.Fatal(err)
	}
	want := hostToWire(reading)
	if hb.Host == nil || *hb.Host != *want {
		t.Fatalf("heartbeat host = %+v, want %+v", hb.Host, want)
	}
}

// The host gate's new package-temperature check and the disk gate hold
// a take back independently of each other -- each crossing logged once
// entering and once leaving, never the other gate's reason -- and
// neither ever touches a lease already running.
func TestHostAndDiskGatesTogether(t *testing.T) {
	var logs []string
	disk := &fakeDiskProbe{free: 100 * (1 << 30)}
	machine := &fakeMachine{reading: MachineReading{Load1: 1, CPUs: 4, PackageTempC: 50, HasTemp: true}}
	r := &Runtime{Disk: disk, Machine: machine, Host: Host{PackageTempMaxC: 90}}
	r.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	r.buildCacheConfig = BuildCacheConfig{MinFreeGB: 50}
	lease := &running{jobID: "j-1", kind: "build", started: time.Now()}
	r.leases.Put("l-1", lease)

	match := func(label, kind, got, want string) {
		t.Helper()
		if want == "" {
			if got != "" {
				t.Fatalf("%s: %s = %q, want none", label, kind, got)
			}
			return
		}
		if !strings.Contains(got, want) {
			t.Fatalf("%s: %s = %q, want it to contain %q", label, kind, got, want)
		}
	}
	check := func(label, wantHost, wantDisk string) {
		t.Helper()
		match(label, "hostGate", r.hostGate(), wantHost)
		r.noteHostGate(r.hostGate())
		match(label, "diskGate", r.diskGate(), wantDisk)
		r.noteDiskGate(r.diskGate())
		if l, ok := r.leases.Get("l-1"); !ok || l != lease {
			t.Fatalf("%s: the running lease was touched by the gates", label)
		}
	}
	countContaining := func(sub string) int {
		n := 0
		for _, l := range logs {
			if strings.Contains(l, sub) {
				n++
			}
		}
		return n
	}

	// quiet to start, and held quiet on a repeat: no log at all
	check("quiet", "", "")
	check("quiet again", "", "")
	if n := len(logs); n != 0 {
		t.Fatalf("quiet start logged %d line(s): %v", n, logs)
	}

	// temperature only: the host enters hot, disk stays quiet
	machine.reading = MachineReading{Load1: 1, CPUs: 4, PackageTempC: 95, HasTemp: true}
	check("temperature only", "host hot: package 95.0°C over 90.0°C", "")
	if n := countContaining("host hot"); n != 1 {
		t.Fatalf("after temperature only, %d host-hot log(s): %v", n, logs)
	}
	if n := countContaining("disk low"); n != 0 {
		t.Fatalf("after temperature only, %d disk log(s), want none: %v", n, logs)
	}

	// disk only: temperature cools (host leaves), disk crosses (disk enters)
	machine.reading = MachineReading{Load1: 1, CPUs: 4, PackageTempC: 50, HasTemp: true}
	disk.free = 10 * (1 << 30)
	check("disk only", "", "disk low at")
	if n := countContaining("host ok"); n != 1 {
		t.Fatalf("after disk only, %d host-ok log(s): %v", n, logs)
	}
	if n := countContaining("disk low"); n != 1 {
		t.Fatalf("after disk only, %d disk-low log(s): %v", n, logs)
	}

	// both: temperature crosses again (host enters again); disk stays
	// low (no new disk log)
	machine.reading = MachineReading{Load1: 1, CPUs: 4, PackageTempC: 95, HasTemp: true}
	check("both", "host hot: package 95.0°C over 90.0°C", "disk low at")
	if n := countContaining("host hot"); n != 2 {
		t.Fatalf("after both, %d host-hot log(s), want 2: %v", n, logs)
	}
	if n := countContaining("disk low"); n != 1 {
		t.Fatalf("after both, %d disk-low log(s), want still 1: %v", n, logs)
	}

	// back under both
	machine.reading = MachineReading{Load1: 1, CPUs: 4, PackageTempC: 50, HasTemp: true}
	disk.free = 100 * (1 << 30)
	check("back under both", "", "")
	if n := countContaining("host ok"); n != 2 {
		t.Fatalf("after back under both, %d host-ok log(s), want 2: %v", n, logs)
	}
	if n := countContaining("disk ok"); n != 1 {
		t.Fatalf("after back under both, %d disk-ok log(s), want 1: %v", n, logs)
	}

	if l, ok := r.leases.Get("l-1"); !ok || l != lease {
		t.Fatal("the running lease was touched by the gates")
	}
}

// Status.DrainLines, given a Host reading and a non-empty HostHistory,
// shows the current load, pressure, memory, disk and temperature and
// the last hour's worst (peak) of each -- independently per metric,
// since no one reading in the window need be worst at all of them.
func TestDrainLinesShowTheHostReadingAndItsPeak(t *testing.T) {
	now := time.Now()
	history := []MachineReading{
		{
			Load1: 1.0, CPUs: 4, MemAvailable: 4 << 30,
			CPUSomeAvg10: 5, CPUSomeAvg60: 6, HasCPUPressure: true,
			MemSomeAvg10: 1, MemSomeAvg60: 2, HasMemPressure: true,
			PackageTempC: 50, HasTemp: true,
			LeaseDisk: DiskReading{FreeBytes: 40 << 30, PercentUsed: 60, Has: true},
			CacheDisk: DiskReading{FreeBytes: 35 << 30, PercentUsed: 65, Has: true},
		},
		{
			// the window's worst reading: hotter, less free memory,
			// more pressure, less free disk
			Load1: 3.0, CPUs: 4, MemAvailable: 1 << 30,
			CPUSomeAvg10: 20, CPUSomeAvg60: 25, HasCPUPressure: true,
			MemSomeAvg10: 10, MemSomeAvg60: 15, HasMemPressure: true,
			PackageTempC: 95, HasTemp: true,
			LeaseDisk: DiskReading{FreeBytes: 5 << 30, PercentUsed: 95, Has: true},
			CacheDisk: DiskReading{FreeBytes: 3 << 30, PercentUsed: 97, Has: true},
		},
		{
			// the latest reading, back to quiet: Status.Host shows this
			// one, not the peak
			Load1: 0.5, CPUs: 4, MemAvailable: 5 << 30,
			CPUSomeAvg10: 2, CPUSomeAvg60: 3, HasCPUPressure: true,
			MemSomeAvg10: 1, MemSomeAvg60: 1, HasMemPressure: true,
			PackageTempC: 48, HasTemp: true,
			LeaseDisk: DiskReading{FreeBytes: 42 << 30, PercentUsed: 58, Has: true},
			CacheDisk: DiskReading{FreeBytes: 38 << 30, PercentUsed: 62, Has: true},
		},
	}
	s := Status{PID: 1, Host: history[len(history)-1], HostHistory: history}
	got := strings.Join(s.DrainLines(now), "\n")

	for _, want := range []string{
		"Host: load 0.50/4 cores", "cpu pressure 2%/3%", "mem 5.0 GB free", "mem pressure 1%/1%",
		"lease disk 42.0 GB free (58.0% used)", "cache disk 38.0 GB free (62.0% used)", "temp 48.0°C",
		"peak last hour: load 3.00/4 cores", "cpu pressure 20%/25%", "mem 1.0 GB free", "mem pressure 10%/15%",
		"lease disk 5.0 GB free (95.0% used)", "cache disk 3.0 GB free (97.0% used)", "temp 95.0°C",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("drain lines\n%s\nlack %q", got, want)
		}
	}

	// no HostHistory: no Host line at all
	s.HostHistory = nil
	if got := strings.Join(s.DrainLines(now), "\n"); strings.Contains(got, "Host:") {
		t.Fatalf("drain lines with no history show a Host line:\n%s", got)
	}
}
