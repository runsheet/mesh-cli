//go:build linux

package nodev1

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// countingHostProbe counts the reads of the probe it wraps.
type countingHostProbe struct {
	probe HostProbe
	mu    sync.Mutex
	reads int
	paths [2]string
}

func (c *countingHostProbe) Read(leasePath, cachePath string) (MachineReading, error) {
	c.mu.Lock()
	c.reads++
	c.paths = [2]string{leasePath, cachePath}
	c.mu.Unlock()
	return c.probe.Read(leasePath, cachePath)
}

func (c *countingHostProbe) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// Run's host monitor reads the fixture probe once at start and once per
// simulated minute, caching each reading and keeping at most the last
// 60, while the host gate holds every take back: no lease is taken, and
// the monitor ticks regardless.
func TestRunReadsTheHostOnceAMinuteWithNoLeaseTaken(t *testing.T) {
	root := hostFixture(t)
	loadAt := func(minute int) {
		mustWrite(t, filepath.Join(root, "proc", "loadavg"), fmt.Sprintf("%d.00 1.00 1.00 1/1 1\n", minute))
	}
	loadAt(0)

	g := newRig(t, &stubSession{})
	g.rt.Host = Host{LoadPerCoreMax: 0.0001}
	g.rt.Machine = &fakeMachine{reading: MachineReading{Load1: 100, CPUs: 1}}
	probe := &countingHostProbe{probe: &ProcHostProbe{Root: root}}
	g.rt.HostProbe = probe
	clock := &fakeClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	ticks := make(chan time.Time)
	g.rt.hostTicks = ticks
	g.offer(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.rt.Run(ctx) }()

	waitFor(t, "the first reading at start", func() bool { return probe.count() == 1 })
	if got := g.rt.HostReading(); got.Load1 != 0 || !got.HasCPUPressure || !got.HasMemPressure || !got.HasTemp {
		t.Fatalf("the reading at start: %+v", got)
	}

	const minutes = 75
	for m := 1; m <= minutes; m++ {
		loadAt(m)
		clock.add(time.Minute)
		ticks <- clock.now()
		waitFor(t, fmt.Sprintf("minute %d's reading", m), func() bool { return g.rt.HostReading().Load1 == float64(m) })
		if got := probe.count(); got != m+1 {
			t.Fatalf("after %d simulated minutes the probe was read %d times, want %d", m, got, m+1)
		}
		history := g.rt.HostHistory()
		if want := min(m+1, hostHistoryMax); len(history) != want {
			t.Fatalf("after %d simulated minutes the history holds %d readings, want %d", m, len(history), want)
		}
		if last := history[len(history)-1]; last.Load1 != float64(m) {
			t.Fatalf("the history's last reading is minute %v, want %d", last.Load1, m)
		}
	}
	history := g.rt.HostHistory()
	if first := history[0].Load1; first != float64(minutes+1-hostHistoryMax) {
		t.Fatalf("the history's oldest reading is minute %v, want %d", first, minutes+1-hostHistoryMax)
	}
	probe.mu.Lock()
	paths := probe.paths
	probe.mu.Unlock()
	if paths != [2]string{g.rt.WorkRoot, g.rt.diskGatePath()} {
		t.Fatalf("the probe read disk at %v, want the lease path %s and the cache path %s", paths, g.rt.WorkRoot, g.rt.diskGatePath())
	}
	if n := len(g.fake.Calls("take")); n != 0 {
		t.Fatalf("%d take call(s) while the host gate held them back", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// With no HostProbe, WatchHost runs no monitor and caches nothing.
func TestWatchHostWithNoProbeDoesNothing(t *testing.T) {
	r := &Runtime{}
	done := make(chan struct{})
	go func() { r.WatchHost(context.Background(), time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchHost did not return with no HostProbe set")
	}
	if reading := r.HostReading(); reading != (MachineReading{}) {
		t.Fatalf("HostReading: %+v, want the zero value", reading)
	}
}
