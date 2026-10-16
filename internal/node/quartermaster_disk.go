package node

import (
	"context"
	"fmt"
	"os"
	"time"
)

// DiskStat is one filesystem's statfs reading: free and total bytes,
// and Device, used to de-duplicate paths that land on the same
// filesystem (two of DiskCheck's Paths often share one).
type DiskStat struct {
	Free, Total uint64
	Device      uint64
}

// StatfsFunc reads one path's filesystem; DiskCheck's default is the
// real statfs(2), injectable for tests.
type StatfsFunc func(path string) (DiskStat, error)

// DiskWarnFree and DiskCriticalFree are DiskCheck's default thresholds
// (goal the-quartermaster-keeps-the-nodes-fit: dl360's disk filled on
// 2026-10-15 and the merge queue deadlocked).
const (
	DiskWarnFree     = 0.15
	DiskCriticalFree = 0.05
)

// DiskCheck is the quartermaster's disk check: one `disk` Finding per
// filesystem among Paths, de-duplicated by device, warn under WarnFree
// free and critical under CriticalFree.
type DiskCheck struct {
	Paths  []string
	Statfs StatfsFunc // the real statfs(2) when nil

	WarnFree     float64 // DiskWarnFree when zero
	CriticalFree float64 // DiskCriticalFree when zero
}

// NewDiskCheck is the disk check over the node's own work root,
// os.TempDir() and its home directory — the paths dl360's disk filled
// across (67G runsheet-* under /tmp, stale mesh-home-* dirs, dead
// checkouts under the work root).
func NewDiskCheck(workRoot string) *DiskCheck {
	home, _ := os.UserHomeDir()
	return &DiskCheck{Paths: []string{workRoot, os.TempDir(), home}}
}

func (c *DiskCheck) warnFree() float64 {
	if c.WarnFree > 0 {
		return c.WarnFree
	}
	return DiskWarnFree
}

func (c *DiskCheck) criticalFree() float64 {
	if c.CriticalFree > 0 {
		return c.CriticalFree
	}
	return DiskCriticalFree
}

func (c *DiskCheck) statfs() StatfsFunc {
	if c.Statfs != nil {
		return c.Statfs
	}
	return realStatfs
}

// Check is the quartermaster Check for disk space: one `disk` Finding
// per filesystem among c.Paths; a later path on a device already
// reported, or one statfs cannot read, is dropped.
func (c *DiskCheck) Check(ctx context.Context, now time.Time) []Finding {
	statfs := c.statfs()
	seen := map[uint64]bool{}
	var out []Finding
	for _, p := range c.Paths {
		if p == "" {
			continue
		}
		st, err := statfs(p)
		if err != nil {
			continue
		}
		if seen[st.Device] {
			continue
		}
		seen[st.Device] = true
		out = append(out, diskFinding(p, st, now, c.warnFree(), c.criticalFree()))
	}
	return out
}

// diskFinding is one filesystem's disk Finding: ok, warn under warnFree
// free, critical under criticalFree free.
func diskFinding(path string, st DiskStat, now time.Time, warnFree, criticalFree float64) Finding {
	var pct float64
	if st.Total > 0 {
		pct = float64(st.Free) / float64(st.Total)
	}
	severity := "ok"
	switch {
	case pct < criticalFree:
		severity = "critical"
	case pct < warnFree:
		severity = "warn"
	}
	return Finding{
		Kind:     "disk",
		Severity: severity,
		Subject:  path,
		Detail:   fmt.Sprintf("%.0f%% free (%d of %d bytes free)", pct*100, st.Free, st.Total),
		Bytes:    int64(st.Free),
		At:       now,
	}
}
