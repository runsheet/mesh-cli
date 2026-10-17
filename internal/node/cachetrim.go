package node

// The shared build cache is held under a cap (goal
// a-host-sessions-scratch-dies-with-the-session, item 4). Every host
// session now shares one GOCACHE under the cache root (BuildCacheDir), and
// nothing but a served-diff validation's Enforce ever trimmed it — so a
// node that ran builds and evaluations alone grew it without bound.
// CacheTrim measures the cache root at the node's first tick and at most
// once an hour after, trims it below TrimFraction of node.yaml's
// validation_cache_max_bytes when it is over (SharedCache.Trim), and
// leaves the reading, with the scratch root's size, for `mesh node
// status` (RunState.WriteCacheSize).

import (
	"context"
	"time"
)

// DefaultCacheTrimEvery is how often CacheTrim measures and trims: an
// hour.
const DefaultCacheTrimEvery = time.Hour

// CacheTrim is the loop's hourly cache trim. Cache is required; the rest
// are optional.
type CacheTrim struct {
	// Cache is the node's shared cache (NewServedDiffCache), its Root and
	// MaxBytes the root measured and the cap held.
	Cache *SharedCache
	// ScratchRoot is the node's scratch root (ScratchRoot), measured
	// beside the cache; "" measures none.
	ScratchRoot string
	// State, when set, is where each reading is left.
	State *RunState
	// Every is the least time between runs; 0 is DefaultCacheTrimEvery.
	Every time.Duration
	// Logf is node.log; nil logs nothing.
	Logf func(format string, args ...any)

	last time.Time
}

// RunIfDue measures, trims and records when Every has passed since the
// last run, or on the first call; it reports whether it ran. A trim is
// logged `cache: trimmed <before> to <after>`.
func (t *CacheTrim) RunIfDue(ctx context.Context, now time.Time) bool {
	if t == nil || t.Cache == nil {
		return false
	}
	every := t.Every
	if every <= 0 {
		every = DefaultCacheTrimEvery
	}
	if !t.last.IsZero() && now.Sub(t.last) < every {
		return false
	}
	t.last = now
	before, after, trimmed, err := t.Cache.Trim(ctx)
	if err != nil {
		t.logf("cache: measure %s: %v", t.Cache.Root, err)
		return true
	}
	if trimmed {
		t.logf("cache: trimmed %s to %s", FormatBytes(before), FormatBytes(after))
	}
	r := CacheReading{Root: t.Cache.Root, Bytes: after, Cap: t.Cache.MaxBytes, At: now.UTC()}
	if t.ScratchRoot != "" {
		r.ScratchRoot, r.ScratchBytes = t.ScratchRoot, dirSize(t.ScratchRoot)
	}
	if t.State != nil {
		if err := t.State.WriteCacheSize(r); err != nil {
			t.logf("cache: record size: %v", err)
		}
	}
	return true
}

func (t *CacheTrim) logf(format string, args ...any) {
	if t.Logf != nil {
		t.Logf(format, args...)
	}
}
