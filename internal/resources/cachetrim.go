package resources

// The cache trim (ADR-044, Mesh v1 PR 3.4): a resource actor that holds
// the node's Go build cache under a cap, off the take loop. It measures
// the cache every Every and, when it is over MaxBytes, takes the cache
// exclusively from the node's scheduler — waiting at most TrimWait for
// the leases building with it to end, while no new lease starts one —
// and only then cleans it. A trim never runs under a live build: the
// 2026-10-28 failures, `go clean -cache` mid-validation failing every Go
// step, cannot happen here. A wait that runs out skips this round and
// says so; the next round tries again.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/scheduler"
)

// Cache trim defaults.
const (
	DefaultTrimEvery = time.Hour
	DefaultTrimWait  = 10 * time.Minute
)

// CacheTrim is the node's cache trim. Root, MaxBytes and Scheduler are
// required.
type CacheTrim struct {
	// Root is the Go build cache (GOCACHE).
	Root string
	// MaxBytes is the cap: a cache over it is cleaned.
	MaxBytes int64
	// Scheduler owns the cache: the trim holds it exclusively.
	Scheduler *scheduler.Scheduler
	// Every is how often the cache is measured; DefaultTrimEvery when
	// zero. TrimWait bounds the wait for the cache; DefaultTrimWait.
	Every    time.Duration
	TrimWait time.Duration
	// Clean empties the cache; `go clean -cache` with GOCACHE=Root when
	// nil.
	Clean func(ctx context.Context, root string) error
	// Logf hears every trim and skip; nothing is logged when nil.
	Logf func(format string, args ...any)
}

// TrimResult is what one round did.
type TrimResult struct {
	Before, After int64
	Trimmed       bool
	// Skipped is why the round did not trim an over-cap cache.
	Skipped string
}

func (t *CacheTrim) logf(format string, args ...any) {
	if t.Logf != nil {
		t.Logf(format, args...)
	}
}

// Run trims once at start and every Every after, until ctx ends.
func (t *CacheTrim) Run(ctx context.Context) {
	every := t.Every
	if every <= 0 {
		every = DefaultTrimEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if _, err := t.Once(ctx); err != nil && ctx.Err() == nil {
			t.logf("cache: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Once is one round: measure, and when over the cap, hold the cache
// exclusively and clean it.
func (t *CacheTrim) Once(ctx context.Context) (TrimResult, error) {
	before, err := DirSize(t.Root)
	if err != nil {
		return TrimResult{}, fmt.Errorf("measuring %s: %w", t.Root, err)
	}
	res := TrimResult{Before: before, After: before}
	if t.MaxBytes <= 0 || before <= t.MaxBytes {
		return res, nil
	}
	wait := t.TrimWait
	if wait <= 0 {
		wait = DefaultTrimWait
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	hold, err := t.Scheduler.Take(wctx, scheduler.Ask{Owner: "cache-trim", Cache: scheduler.CacheExclusive, TTL: wait + time.Hour})
	if err != nil {
		res.Skipped = fmt.Sprintf("the cache is %s of %s but builds held it for %s: %v", FormatBytes(before), FormatBytes(t.MaxBytes), wait, err)
		t.logf("cache: trim skipped: %s", res.Skipped)
		return res, nil
	}
	defer hold.Release()
	clean := t.Clean
	if clean == nil {
		clean = goCleanCache
	}
	if err := clean(ctx, t.Root); err != nil {
		return res, fmt.Errorf("cleaning %s: %w", t.Root, err)
	}
	after, err := DirSize(t.Root)
	if err != nil {
		return res, fmt.Errorf("measuring %s: %w", t.Root, err)
	}
	res.After, res.Trimmed = after, true
	t.logf("cache: trimmed %s from %s to %s (cap %s), holding it alone", t.Root, FormatBytes(before), FormatBytes(after), FormatBytes(t.MaxBytes))
	return res, nil
}

// goCleanCache is `go clean -cache` on root.
func goCleanCache(ctx context.Context, root string) error {
	cmd := exec.CommandContext(ctx, "go", "clean", "-cache")
	cmd.Env = append(os.Environ(), "GOCACHE="+root, "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go clean -cache: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DirSize is the bytes of the regular files under root; 0 when root
// does not exist.
func DirSize(root string) (int64, error) {
	var n int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n, err
}

// FormatBytes is n in binary units.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
