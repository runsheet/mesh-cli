package nodev1

import (
	"context"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"projectmesh.io/mesh-cli/internal/resources"
)

// The build cache watcher (ADR-044 amendment, goal
// a-nodes-build-cache-stays-under-its-size): node.yaml's own top-level
// `cache: {max_gb, min_free_gb}` section, read directly here as
// capacitywatch.go reads `v1.capacity`, since nodev1 may not import
// internal/node (guard_test.go) — the legacy node's own schema names
// no top-level `cache:` key, so this is nodev1's alone. max_gb bounds
// the node's one Go build cache (BuildCache), evicted live by
// WatchBuildCache through resources.EvictLRU — by entry, never taking
// the scheduler, so it runs alongside a live build unlike the
// wholesale CacheTrim. min_free_gb is diskGate's floor.

// DefaultCacheFreeShare is what share of the disk free at the cache's
// own path max_gb defaults to, node.yaml naming none.
const DefaultCacheFreeShare = 0.6

// DefaultMinFreeGB is min_free_gb, node.yaml naming none.
const DefaultMinFreeGB = 50.0

// BuildCacheConfig is node.yaml's `cache` section, live.
type BuildCacheConfig struct {
	// MaxGB is the cap WatchBuildCache evicts the build cache down
	// toward.
	MaxGB float64
	// MinFreeGB is diskGate's floor: at or under it, the node takes no
	// new lease.
	MinFreeGB float64
}

// MaxBytes is MaxGB in bytes, for resources.EvictLRU's cap; <= 0 when
// MaxGB is.
func (c BuildCacheConfig) MaxBytes() int64 {
	return int64(c.MaxGB * (1 << 30))
}

// nodeYAMLCache is node.yaml's `cache` block.
type nodeYAMLCache struct {
	Cache *nodeYAMLCacheFields `yaml:"cache"`
}

type nodeYAMLCacheFields struct {
	MaxGB     *float64 `yaml:"max_gb"`
	MinFreeGB *float64 `yaml:"min_free_gb"`
}

// readCacheYAML is node.yaml's `cache` block as the file names it,
// pointers left nil for a field the block leaves out, a file with no
// such block, or a file that does not exist.
func readCacheYAML(path string) (nodeYAMLCacheFields, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nodeYAMLCacheFields{}, nil
		}
		return nodeYAMLCacheFields{}, err
	}
	var cfg nodeYAMLCache
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nodeYAMLCacheFields{}, err
	}
	if cfg.Cache == nil {
		return nodeYAMLCacheFields{}, nil
	}
	return *cfg.Cache, nil
}

// ReadBuildCacheConfig reads node.yaml's `cache` section: a field it
// leaves out defaults — max_gb to DefaultCacheFreeShare of freeBytes's
// reading of the disk the cache lives on, min_free_gb to
// DefaultMinFreeGB — so a node with no cache section at all still
// bounds its build cache and backs off a nearly-full disk. freeBytes
// is called only when max_gb needs defaulting; nil reads as 0 free.
func ReadBuildCacheConfig(path string, freeBytes func() (uint64, error)) (BuildCacheConfig, error) {
	fields, err := readCacheYAML(path)
	if err != nil {
		return BuildCacheConfig{}, err
	}
	var cfg BuildCacheConfig
	if fields.MaxGB != nil {
		cfg.MaxGB = *fields.MaxGB
	} else {
		var free uint64
		if freeBytes != nil {
			free, err = freeBytes()
			if err != nil {
				return BuildCacheConfig{}, err
			}
		}
		cfg.MaxGB = float64(free) / (1 << 30) * DefaultCacheFreeShare
	}
	if fields.MinFreeGB != nil {
		cfg.MinFreeGB = *fields.MinFreeGB
	} else {
		cfg.MinFreeGB = DefaultMinFreeGB
	}
	return cfg, nil
}

// pinBuildCache sets BuildCache, left "", to the path every lease
// mounts as GOCACHE (buildCacheDir: WorkRoot/build-cache) and makes it,
// before Run starts WatchBuildCache: left "", evictBuildCache evicted
// nothing and diskGate read WorkRoot, while every lease filled
// WorkRoot/build-cache unbounded (dl360, 2026-11-09: 456G against a
// default cap near 175G, no eviction ever logged).
func (r *Runtime) pinBuildCache() {
	if r.BuildCache == "" && r.WorkRoot != "" {
		r.BuildCache = r.buildCacheDir()
	}
	if r.BuildCache == "" {
		return
	}
	if err := os.MkdirAll(r.BuildCache, 0o700); err != nil {
		r.logf("nodev1: build cache: %v", err)
	}
}

// WatchBuildCache is the node.yaml watcher for the node's build cache:
// it rereads node.yaml's cache config with read every poll and at once
// whenever reload fires, keeps it live as r.buildCacheConfig —
// diskGate's floor, and this method's own cap — and evicts
// r.BuildCache's least-recently-used entries whenever it is over the
// cap. A read that errors changes nothing and is logged once.
func (r *Runtime) WatchBuildCache(ctx context.Context, read func() (BuildCacheConfig, error), poll time.Duration, reload <-chan struct{}) {
	if read == nil {
		return
	}
	if poll <= 0 {
		poll = time.Minute
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	started := false
	apply := func() {
		cfg, err := read()
		if err != nil {
			r.logf("nodev1: build cache: node.yaml: %v", err)
			return
		}
		r.mu.Lock()
		r.buildCacheConfig = cfg
		r.mu.Unlock()
		if !started {
			started = true
			r.logf("nodev1: build cache: watching %s every %s: cap %.1f GB, disk floor %.1f GB", r.BuildCache, poll, cfg.MaxGB, cfg.MinFreeGB)
		}
		r.evictBuildCache(cfg)
	}
	apply()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			apply()
		case <-reload:
			apply()
		}
	}
}

// evictBuildCache evicts r.BuildCache's least-recently-used entries
// toward cfg's cap when it is over it (resources.EvictLRU): by entry,
// never taking r.Scheduler, so it runs alongside whatever is building
// against the same cache. "" BuildCache or a non-positive cap evicts
// nothing.
func (r *Runtime) evictBuildCache(cfg BuildCacheConfig) {
	if r.BuildCache == "" {
		return
	}
	maxBytes := cfg.MaxBytes()
	if maxBytes <= 0 {
		return
	}
	res, err := resources.EvictLRU(r.BuildCache, maxBytes)
	if err != nil {
		r.logf("nodev1: build cache: evicting %s: %v", r.BuildCache, err)
		return
	}
	if res.Entries > 0 {
		r.logf("nodev1: build cache: evicted %d entries at %s (%s -> %s, cap %s)", res.Entries, r.BuildCache, resources.FormatBytes(res.Before), resources.FormatBytes(res.After), resources.FormatBytes(maxBytes))
	}
}
