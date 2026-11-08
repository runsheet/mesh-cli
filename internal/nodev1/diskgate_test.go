package nodev1

import (
	"strings"
	"testing"
	"time"
)

type fakeDiskProbe struct {
	free uint64
}

func (f *fakeDiskProbe) FreeBytes(string) (uint64, error) {
	return f.free, nil
}

func linesContain(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestDiskGateRefusesALeaseUnderTheFloorAndNamesItInStatus(t *testing.T) {
	probe := &fakeDiskProbe{free: 10 * (1 << 30)}
	r := &Runtime{Disk: probe}
	r.buildCacheConfig = BuildCacheConfig{MinFreeGB: 50}

	reason := r.diskGate()
	if reason == "" {
		t.Fatal("diskGate: want a reason under the floor, got none")
	}
	r.noteDiskGate(reason)

	st := r.Status()
	if st.DiskGate == "" {
		t.Fatal("Status().DiskGate: want the reason, got none")
	}
	lines := st.DrainLines(time.Now())
	if !linesContain(lines, st.DiskGate) {
		t.Fatalf("DrainLines: want a line naming %q, got %v", st.DiskGate, lines)
	}

	// back over the floor clears both
	probe.free = 100 * (1 << 30)
	reason = r.diskGate()
	if reason != "" {
		t.Fatalf("diskGate: want no reason over the floor, got %q", reason)
	}
	r.noteDiskGate(reason)

	st = r.Status()
	if st.DiskGate != "" {
		t.Fatalf("Status().DiskGate: want none once over the floor, got %q", st.DiskGate)
	}
	lines = st.DrainLines(time.Now())
	if linesContain(lines, "Disk:") {
		t.Fatalf("DrainLines: want no disk line once over the floor, got %v", lines)
	}
}
