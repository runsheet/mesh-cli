package node

import (
	"context"
	"testing"
	"time"
)

// The disk check reports ok at 50% free, warn at 10% and critical at
// 3% (the defaults: warn under 15%, critical under 5%), and two paths
// on the same device land in one finding (goal
// the-quartermaster-keeps-the-nodes-fit, evidence: dl360's disk filled
// on 2026-10-15 across /tmp, the work root and the home directory,
// often one filesystem).
func TestDiskCheckThresholds(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	fake := func(stats map[string]DiskStat) StatfsFunc {
		return func(path string) (DiskStat, error) { return stats[path], nil }
	}

	cases := []struct {
		name     string
		free     uint64
		total    uint64
		severity string
	}{
		{"ok", 50, 100, "ok"},
		{"warn", 10, 100, "warn"},
		{"critical", 3, 100, "critical"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &DiskCheck{
				Paths:  []string{"/work"},
				Statfs: fake(map[string]DiskStat{"/work": {Free: tc.free, Total: tc.total, Device: 1}}),
			}
			got := c.Check(context.Background(), now)
			if len(got) != 1 {
				t.Fatalf("Check() = %+v, want 1 finding", got)
			}
			if got[0].Kind != "disk" {
				t.Fatalf("Kind = %q, want disk", got[0].Kind)
			}
			if got[0].Severity != tc.severity {
				t.Fatalf("Severity = %q, want %q", got[0].Severity, tc.severity)
			}
			if got[0].Bytes != int64(tc.free) {
				t.Fatalf("Bytes = %d, want %d", got[0].Bytes, tc.free)
			}
		})
	}
}

// Two paths that land on the same device (statfs's dedup key) report
// as one finding, not two.
func TestDiskCheckDedupesByDevice(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	stats := map[string]DiskStat{
		"/work":    {Free: 10, Total: 100, Device: 7},
		"/tmp":     {Free: 10, Total: 100, Device: 7},
		"/home/op": {Free: 90, Total: 100, Device: 9},
	}
	c := &DiskCheck{
		Paths:  []string{"/work", "/tmp", "/home/op"},
		Statfs: func(path string) (DiskStat, error) { return stats[path], nil },
	}
	got := c.Check(context.Background(), now)
	if len(got) != 2 {
		t.Fatalf("Check() = %+v, want 2 findings (/work and /tmp share a device)", got)
	}
	if got[0].Subject != "/work" {
		t.Fatalf("the first path on a device wins the finding: Subject = %q, want /work", got[0].Subject)
	}
	if got[1].Subject != "/home/op" {
		t.Fatalf("Subject = %q, want /home/op", got[1].Subject)
	}
}
