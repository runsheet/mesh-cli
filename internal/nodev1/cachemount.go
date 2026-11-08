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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
	var mounts []sandbox.Mount
	var env []string

	if !r.cacheMountIsOff() {
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
	}

	buildCache := r.buildCacheDir()
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

// cacheMountProbeTimeout bounds realCacheMountProbe's own pod and agent
// calls, independent of ctx: a probe is one throwaway pod at start-up,
// never a reason the first claim waits any real length of time on a
// slow or wedged runtime.
const cacheMountProbeTimeout = 30 * time.Second

// probeCacheMountOnce checks once, at node start-up, whether the
// runtime refuses a tenant cache's `--mount type=overlay`
// (*sandbox.CacheMountUnsupportedError — never a *sandbox.RuntimeError,
// which already stops the node claiming on its own, below): refused,
// every lease this process runs from then on gets no tenant cache
// overlay at all (cacheMountIsOff, leaseCacheMounts above) — logged
// "cache mount off" exactly once — rather than every lease
// re-attempting, and failing, the same mount (goal
// the-warm-cache-mounts-on-the-nodes-podman, item 2: a refusing runtime
// never holds a lease back for this). A probe error that is not that
// refusal changes nothing: a lease's own mount attempt is still the
// real answer, logged there as it already is.
//
// With no CacheMountProbe the default only ever runs against the real
// podman (r.Sandbox a *sandbox.Podman): the overlay refusal this probe
// looks for is podman's own, and nothing else — not a Runtime a test
// wires directly to exercise a lease's own pod and agent — can produce
// it, so nothing else is probed.
func (r *Runtime) probeCacheMountOnce(ctx context.Context) {
	r.mu.Lock()
	if r.cacheMountProbed {
		r.mu.Unlock()
		return
	}
	r.cacheMountProbed = true
	r.mu.Unlock()

	probe := r.CacheMountProbe
	if probe == nil {
		if _, ok := r.Sandbox.(*sandbox.Podman); !ok {
			return
		}
		probe = r.realCacheMountProbe
	}
	ctx, cancel := context.WithTimeout(ctx, cacheMountProbeTimeout)
	defer cancel()
	refused, err := probe(ctx)
	if err != nil {
		r.logf("nodev1: cache mount probe: %v", err)
	}
	if !refused {
		return
	}
	r.mu.Lock()
	r.cacheMountOff = true
	r.mu.Unlock()
	r.logf("nodev1: cache mount off")
}

// cacheMountIsOff reports whether probeCacheMountOnce found the
// runtime refuses a cache's overlay mount.
func (r *Runtime) cacheMountIsOff() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cacheMountOff
}

// realCacheMountProbe is probeCacheMountOnce's default check, run
// against r.Sandbox directly rather than through a lease: a throwaway
// pod's agent mounts a throwaway overlay the same way leaseCacheMounts
// mounts a tenant cache (Mount.Overlay), and refused is true only for
// *sandbox.CacheMountUnsupportedError.
func (r *Runtime) realCacheMountProbe(ctx context.Context) (refused bool, err error) {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	suffix := hex.EncodeToString(rnd[:])

	root := filepath.Join(r.WorkRoot, ".cache-probe", suffix)
	lower := filepath.Join(root, "lower")
	upper := filepath.Join(root, "upper")
	work := filepath.Join(root, "work")
	for _, dir := range []string{lower, upper, work} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, fmt.Errorf("nodev1: cache mount probe: %w", err)
		}
	}
	defer os.RemoveAll(root)

	pod, err := r.Sandbox.CreatePod(ctx, sandbox.PodSpec{
		Name:    "mesh-cache-probe-" + suffix,
		Image:   r.PodImage,
		Network: sandbox.NetworkPolicy{None: true},
	})
	if err != nil {
		return false, fmt.Errorf("nodev1: cache mount probe: pod: %w", err)
	}
	defer func() { _ = r.Sandbox.Remove(context.WithoutCancel(ctx), pod) }()

	_, err = r.Sandbox.RunAgent(ctx, pod, sandbox.AgentSpec{
		Name:    "mesh-cache-probe-agent-" + suffix,
		Image:   r.PodImage,
		Mounts:  []sandbox.Mount{{Source: lower, Target: lower, Overlay: true, Upper: upper, Work: work}},
		Command: []string{"true"},
	})
	var unsupported *sandbox.CacheMountUnsupportedError
	if errors.As(err, &unsupported) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("nodev1: cache mount probe: %w", err)
	}
	return false, nil
}

// buildCacheDir is the node's one Go build cache: BuildCache, else
// WorkRoot/build-cache — the path every lease mounts as GOCACHE, and
// the path Run pins BuildCache to before the build-cache watcher
// starts, so its eviction and diskGate read the cache the leases
// actually fill.
func (r *Runtime) buildCacheDir() string {
	if r.BuildCache != "" {
		return r.BuildCache
	}
	return filepath.Join(r.WorkRoot, "build-cache")
}
