package nodev1

// cachemount.go: a lease's pod starts from a warm cache (goal
// a-v1-lease-starts-from-a-warm-cache, item 2): its tenant's GOMODCACHE,
// npm cache and pnpm store (cache.go, item 1) are mounted as overlays
// whose read-only lower is the tenant's shared cache and whose upper and
// work dirs are this lease's own, under its job directory, so two leases
// of the same tenant running at once each write without racing the
// other, and no mount or env of one lease ever names another lease's or
// another tenant's path. The Go build cache (GOCACHE) is the exception:
// content-addressed and safe to share, it is one plain read-write mount
// whose source is the same for every tenant's every lease.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// leaseCacheMounts are the mounts and the environment that put b's
// lease's pod on its tenant's warm cache: an overlay per cache (gomod,
// npm, pnpm), this lease's own upper and work dirs under leaseDir, and
// the node's one shared GOCACHE mount. A tenant cache that cannot be
// made is logged and left out — a cold cache, never a reason to refuse
// the lease.
func (r *Runtime) leaseCacheMounts(b *meshapi.Brief, leaseDir string) ([]sandbox.Mount, []string) {
	if r.cacheMountOff.Load() {
		return nil, nil
	}

	var mounts []sandbox.Mount
	var env []string

	tc := r.TenantCache
	if tc == nil {
		tc = NewTenantCache(r.WorkRoot, CacheConfig{})
	}
	if dirs, err := tc.Dirs(b.Tenant); err != nil {
		r.logf("nodev1: lease %s: tenant cache: %v", b.LeaseID, err)
	} else {
		overlays := []struct {
			name, source, envVar string
		}{
			{"gomod", dirs.GoMod, "GOMODCACHE"},
			{"npm", dirs.Npm, "npm_config_cache"},
			{"pnpm", dirs.Pnpm, "pnpm_config_store_dir"},
		}
		for _, o := range overlays {
			m, err := r.overlayMount(leaseDir, o.name, o.source)
			if err != nil {
				r.logf("nodev1: lease %s: tenant cache: %v", b.LeaseID, err)
				continue
			}
			mounts = append(mounts, m)
			env = append(env, o.envVar+"="+o.source)
		}
	}

	buildCache := r.BuildCache
	if buildCache == "" {
		buildCache = filepath.Join(r.WorkRoot, "build-cache")
	}
	if err := os.MkdirAll(buildCache, 0o700); err != nil {
		r.logf("nodev1: lease %s: build cache: %v", b.LeaseID, err)
	} else {
		mounts = append(mounts, sandbox.Mount{Source: buildCache, Target: buildCache})
		env = append(env, "GOCACHE="+buildCache)
	}

	return mounts, env
}

// overlayMount is a tenant cache overlay of name beside leaseDir, source
// its read-only lower (also its Target, so every tool sees the same
// path whichever tenant it belongs to): this lease's own upper and work
// dirs, made here, under leaseDir/cache/<name>, never another lease's.
func (r *Runtime) overlayMount(leaseDir, name, source string) (sandbox.Mount, error) {
	base := filepath.Join(leaseDir, "cache", name)
	upper := filepath.Join(base, "upper")
	work := filepath.Join(base, "work")
	for _, dir := range []string{upper, work} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return sandbox.Mount{}, fmt.Errorf("nodev1: overlay %s: %w", name, err)
		}
	}
	return sandbox.Mount{Source: source, Target: source, Overlay: true, Upper: upper, Work: work}, nil
}

// probeCacheMountOnce checks, once per runtime, whether the node's
// cache mount actually works (ProbeCacheMount): never per lease, and
// never a reason to hold a lease back, only to decide what
// leaseCacheMounts offers every lease after. r.Sandbox nil (no pods)
// skips it, there being no pod to mount into. A refusal is logged once
// ("cache mount off", cacheMountOff sticky for the runtime's life) and
// turns leaseCacheMounts into a no-op from then on; a pass leaves the
// cache mounts in force.
func (r *Runtime) probeCacheMountOnce(ctx context.Context) {
	if r.Sandbox == nil || r.cacheMountOff.Load() {
		return
	}
	probe := r.probeCacheMount
	if probe == nil {
		probe = r.realProbeCacheMount
	}
	if err := probe(ctx); err != nil {
		if r.cacheMountOff.CompareAndSwap(false, true) {
			r.logf("nodev1: cache mount off: %v", err)
		}
	}
}

// realProbeCacheMount is probeCacheMountOnce's default: a throwaway
// lower/upper/work/merged under WorkRoot, removed after, through
// sandbox.ProbeCacheMount against r.Sandbox and r.PodImage.
func (r *Runtime) realProbeCacheMount(ctx context.Context) error {
	base, err := os.MkdirTemp(r.WorkRoot, "cache-probe-*")
	if err != nil {
		return fmt.Errorf("nodev1: cache mount probe: %w", err)
	}
	defer os.RemoveAll(base)
	lower := filepath.Join(base, "lower")
	upper := filepath.Join(base, "upper")
	work := filepath.Join(base, "work")
	merged := filepath.Join(base, "merged")
	for _, dir := range []string{lower, upper, work, merged} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("nodev1: cache mount probe: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(lower, "lower.txt"), []byte("probe"), 0o600); err != nil {
		return fmt.Errorf("nodev1: cache mount probe: %w", err)
	}
	return sandbox.ProbeCacheMount(ctx, r.Sandbox, r.PodImage, lower, upper, work, merged)
}
