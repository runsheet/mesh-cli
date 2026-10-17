package node

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/sandbox"
)

// ScratchMinAge is ScratchCheck's default minimum age: an entry whose
// newest mtime anywhere inside is younger than this might still be in
// the middle of being written by a session whose workspace InUse does
// not yet name (goal the-quartermaster-keeps-the-nodes-fit).
const ScratchMinAge = 6 * time.Hour

// ScratchWarnBytes is ScratchCheck's default warn threshold: dl360's
// disk filled on 2026-10-15 behind 67G of runsheet-* left under /tmp.
const ScratchWarnBytes int64 = 20 << 30

// ScratchPrefixes are the mesh-owned entry name globs ScratchCheck
// reaps by default, matched directly under its TempDir.
var ScratchPrefixes = []string{"runsheet-*", "latchkey-*", "go-build*", "mesh-*"}

// ScratchCheck is the quartermaster's scratch check (goal
// the-quartermaster-keeps-the-nodes-fit): it reaps stale mesh-owned
// leftovers directly under TempDir, and, when Sandbox is set, runs the
// sandbox's own home sweep too — replacing the dl360 cron jobs
// /etc/cron.d/mesh-tmp-clean and /etc/cron.hourly/mesh-home-sweep.
type ScratchCheck struct {
	TempDir  string        // os.TempDir() when empty
	Prefixes []string      // ScratchPrefixes when empty
	MinAge   time.Duration // ScratchMinAge when zero

	// WarnBytes is the leftover threshold — the bytes of entries past
	// MinAge that InUse keeps on disk — past which the finding warns.
	WarnBytes int64 // ScratchWarnBytes when zero

	// InUse says a path is a live session's workspace or scratch path:
	// never removed, whatever its age.
	InUse func(path string) bool

	// Sandbox and Sessions, when Sandbox is not nil, run the sandbox's
	// own home sweep on every run — not only at node start — keeping
	// every session Sessions returns, as node.SweepSandbox does at
	// start.
	Sandbox  *sandbox.Sandbox
	Sessions func() []FoundSession
	Logf     func(string, ...any)
}

func (c *ScratchCheck) tempDir() string {
	if c.TempDir != "" {
		return c.TempDir
	}
	return os.TempDir()
}

func (c *ScratchCheck) prefixes() []string {
	if len(c.Prefixes) > 0 {
		return c.Prefixes
	}
	return ScratchPrefixes
}

func (c *ScratchCheck) minAge() time.Duration {
	if c.MinAge > 0 {
		return c.MinAge
	}
	return ScratchMinAge
}

func (c *ScratchCheck) warnBytes() int64 {
	if c.WarnBytes > 0 {
		return c.WarnBytes
	}
	return ScratchWarnBytes
}

func (c *ScratchCheck) logf() func(string, ...any) {
	if c.Logf != nil {
		return c.Logf
	}
	return func(string, ...any) {}
}

func (c *ScratchCheck) inUse(path string) bool {
	return c.InUse != nil && c.InUse(path)
}

// Check is the quartermaster Check for scratch: one `scratch` Finding
// naming what it reaped and what it left, and one more `scratch` warn
// Finding per path a removal failed on.
func (c *ScratchCheck) Check(ctx context.Context, now time.Time) []Finding {
	tempDir := c.tempDir()
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return []Finding{{Kind: "scratch", Severity: "warn", Subject: tempDir, Detail: fmt.Sprintf("list: %v", err), At: now}}
	}

	minAge := c.minAge()
	prefixes := c.prefixes()
	var reaped, leftOver int64
	var reapedCount, inUseCount int
	var failures []Finding
	for _, e := range entries {
		if !matchesAny(e.Name(), prefixes) || !e.IsDir() {
			// Not matched, or a symlink (os.ReadDir's DirEntry reports
			// the entry's own type, never the target's): left alone,
			// never followed.
			continue
		}
		if strings.HasPrefix(e.Name(), sandbox.HomePrefix) {
			// mesh-home-* is the sandbox's own: the sweep below knows
			// which one a running container still mounts, which a
			// bare InUse path hook does not.
			continue
		}
		info, err := e.Info()
		if err != nil || !ownedByNodeUID(info) {
			continue
		}
		path := filepath.Join(tempDir, e.Name())
		newest, err := newestModTime(path)
		if err != nil || now.Sub(newest) < minAge {
			continue
		}
		size := dirSize(path)
		if c.inUse(path) {
			leftOver += size
			inUseCount++
			continue
		}
		makeRemovable(path)
		if err := os.RemoveAll(path); err != nil {
			failures = append(failures, Finding{Kind: "scratch", Severity: "warn", Subject: path, Detail: fmt.Sprintf("remove: %v", err), At: now})
			leftOver += size
			continue
		}
		reaped += size
		reapedCount++
	}

	detail := fmt.Sprintf("reaped %d dirs, %s; %d in use kept", reapedCount, humanBytes(reaped), inUseCount)
	if c.Sandbox != nil {
		var found []FoundSession
		if c.Sessions != nil {
			found = c.Sessions()
		}
		res := SweepSandbox(ctx, c.Sandbox, found, c.logf())
		detail += fmt.Sprintf("; sandbox: %s", res.Line())
	}

	severity := "ok"
	if leftOver > c.warnBytes() {
		severity = "warn"
	}
	main := Finding{
		Kind:      "scratch",
		Severity:  severity,
		Subject:   tempDir,
		Detail:    detail,
		Bytes:     leftOver,
		Reclaimed: reaped,
		At:        now,
	}
	return append([]Finding{main}, failures...)
}

// matchesAny is name matching one of patterns (filepath.Match globs).
func matchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, err := filepath.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// newestModTime is the newest mtime of root or anything inside it,
// never following a symlink: fs.WalkDir reads a symlink's own entry,
// not its target, and never descends into it.
func newestModTime(root string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if t := info.ModTime(); t.After(newest) {
			newest = t
		}
		return nil
	})
	return newest, err
}

// dirSize sums the size of every regular file under root; a walk
// error is taken as read, not retried.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// makeRemovable chmods u+w onto every directory under root that lacks
// it — a Go module cache's read-only package directories among them —
// so os.RemoveAll can clear it; a chmod that fails is left to
// RemoveAll's own error.
func makeRemovable(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if mode := info.Mode().Perm(); mode&0200 == 0 {
			_ = os.Chmod(path, mode|0700)
		}
		return nil
	})
}

// humanBytes renders n in the largest unit that keeps it at least 1,
// one decimal place (41.2G) — the quartermaster's findings never show
// a bare byte count for anything this size.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}
