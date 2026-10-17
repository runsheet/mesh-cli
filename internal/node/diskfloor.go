package node

import (
	"fmt"
)

// The disk floor (goal a-node-low-on-disk-claims-nothing-and-a-missing-
// tool-is-named; the disk hour, 2026-10-15 09:38–09:45, when nodes kept
// claiming with almost no free disk): every tick, before any claim, the
// loop reads the free space on node.yaml's disk.path — the work root
// when left out — through the quartermaster's StatfsFunc. Under
// disk.free_min_gb it claims nothing of any kind — build, evaluation,
// rebase, plan, research, merge batch or main validation — and says so
// once as it enters the state, `disk_low: …`, and once as it leaves it,
// `disk ok: …`. A node that starts under the floor says it at its first
// tick. Running sessions are never touched.

// diskState is the floor as last judged: whether free space was under
// it, and the Finding that says so while it is.
type diskState struct {
	low     bool
	finding *Finding
	// readErr is the last statfs error said, so a failing read is
	// logged once until it changes.
	readErr string
}

// DiskThresholds is node.yaml's `disk:` block in force, its floor
// defaulted: as last reloaded, else Cfg's.
func (l *Loop) DiskThresholds() DiskConfig {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.diskSet {
		return l.disk
	}
	if l.Cfg == nil {
		return DiskConfig{}.Effective()
	}
	return l.Cfg.Disk.Effective()
}

// diskPath is the path the floor reads: disk.path, else the work root;
// "" is not judged.
func (l *Loop) diskPath(d DiskConfig) string {
	if d.Path != "" {
		return d.Path
	}
	return l.WorkRoot
}

// diskLow says free space was under the floor at the last tick: claim
// nothing.
func (l *Loop) diskLow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.diskState.low
}

// DiskFindings is the floor's Finding while the node is under it — kind
// disk_low, critical, its subject the path and its bytes the free space
// — else none: the quartermaster's shape, for its report.
func (l *Loop) DiskFindings() []Finding {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.diskState.finding == nil {
		return nil
	}
	return []Finding{*l.diskState.finding}
}

// diskFloor reads the free space on the floor's path and judges it,
// logging a crossing either way once. A failed read keeps the last
// judgement and is said once per distinct error.
func (l *Loop) diskFloor() {
	d := l.DiskThresholds()
	path := l.diskPath(d)
	if path == "" {
		return
	}
	statfs := l.Statfs
	if statfs == nil {
		statfs = realStatfs
	}
	st, err := statfs(path)
	l.mu.Lock()
	ds := &l.diskState
	if err != nil {
		say := err.Error() != ds.readErr
		ds.readErr = err.Error()
		l.mu.Unlock()
		if say {
			l.logf("disk: cannot read %s: %v; keeping the last judgement", path, err)
		}
		return
	}
	ds.readErr = ""
	floor := d.FloorBytes()
	low := st.Free < floor
	was := ds.low
	ds.low = low
	var line string
	if low {
		line = fmt.Sprintf("disk_low: %s %s free < floor %s; claiming nothing", path, gib(st.Free), gib(floor))
		ds.finding = &Finding{Kind: "disk_low", Severity: "critical", Subject: path, Detail: line, Bytes: int64(st.Free), At: l.now()}
	} else {
		ds.finding = nil
		line = fmt.Sprintf("disk ok: %s %s free >= floor %s; claiming again", path, gib(st.Free), gib(floor))
	}
	l.mu.Unlock()
	if low != was {
		l.logf("%s", line)
	}
}

// gib is a byte count in GiB to one decimal, as `3.2GiB`.
func gib(b uint64) string {
	return fmt.Sprintf("%.1fGiB", float64(b)/(1<<30))
}
