package nodev1

// config.go: defaults for the per-tenant cache (cache.go), the way
// internal/resources/cachetrim.go names DefaultTrimEvery and
// DefaultTrimWait beside the type they bound.

import "path/filepath"

// DefaultTenantCacheMaxBytes is the per-tenant cap Trim holds each of
// GoMod, Npm and Pnpm under when the runtime names none of its own.
const DefaultTenantCacheMaxBytes = 2 << 30 // 2 GiB

// CacheConfig is the per-tenant cache's settings: Root, under the
// node's work root when empty, and MaxBytes, DefaultTenantCacheMaxBytes
// when <= 0.
type CacheConfig struct {
	Root     string
	MaxBytes int64
}

// NewTenantCache answers a TenantCache for workRoot and cfg, with
// CacheConfig's defaults applied.
func NewTenantCache(workRoot string, cfg CacheConfig) *TenantCache {
	root := cfg.Root
	if root == "" {
		root = filepath.Join(workRoot, "tenant-cache")
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultTenantCacheMaxBytes
	}
	return &TenantCache{Root: root, MaxBytes: maxBytes}
}
