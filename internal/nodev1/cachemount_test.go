package nodev1

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// openTestLeasePod opens leaseID's pod for tenant on a shared Runtime
// (one node, several tenants' leases), on the fake runtime, in the
// git checkout ws (so Session.Mounts can read it).
func openTestLeasePod(t *testing.T, rt *Runtime, tenant, leaseID, ws string) *leasePod {
	t.Helper()
	b := &meshapi.Brief{JobID: "job-" + leaseID, LeaseID: leaseID, LeaseN: 1, Kind: "validate",
		Product: "runsheet", Tenant: tenant, Resources: meshapi.Resources{CPUs: 1, MemGB: 1}}
	ctx, cancel := context.WithCancel(context.Background())
	var closers []func() error
	t.Cleanup(func() {
		cancel()
		for _, c := range closers {
			c()
		}
	})
	lp, err := rt.openLeasePod(ctx, b, ws, nil, func(_ string, c func() error) { closers = append(closers, c) })
	if err != nil {
		t.Fatalf("openLeasePod(%s): %v", tenant, err)
	}
	return lp
}

func testRuntime(t *testing.T) *Runtime {
	t.Helper()
	rt := &Runtime{WorkRoot: t.TempDir(), Logf: t.Logf}
	rt.Pods = &sandbox.Sandbox{Runtime: &sandbox.Fake{}, Logf: t.Logf, Config: sandbox.Config{
		Image: "mesh-node:test", Limits: sandbox.Limits{CPUs: 2, Memory: "2g"},
		Registries: sandbox.DefaultRegistries, ServiceTimeout: 5 * time.Second,
	}}
	return rt
}

// renderedMounts is every mount and env var of lp's session, rendered
// as text, for a substring check that no path of one lease's cache
// leaks into another's.
func renderedMounts(lp *leasePod, workspace string) string {
	var b strings.Builder
	for _, m := range lp.sess.Mounts(workspace) {
		fmt.Fprintf(&b, "%+v\n", m)
	}
	for _, e := range lp.sess.Env(nil) {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	return b.String()
}

func overlayMountNamed(t *testing.T, mounts []sandbox.Mount, source string) sandbox.Mount {
	t.Helper()
	for _, m := range mounts {
		if m.Overlay && m.Source == source {
			return m
		}
	}
	t.Fatalf("no overlay mount with source %s among %+v", source, mounts)
	return sandbox.Mount{}
}

func TestPodMountsScopeCacheByTenant(t *testing.T) {
	rt := testRuntime(t)
	wsA := t.TempDir()
	wsB := t.TempDir()
	for _, ws := range []string{wsA, wsB} {
		if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
	}

	lpA := openTestLeasePod(t, rt, "acme", "lease-a", wsA)
	lpB := openTestLeasePod(t, rt, "globex", "lease-b", wsB)

	tc := NewTenantCache(rt.WorkRoot, CacheConfig{})
	dirsA, err := tc.Dirs("acme")
	if err != nil {
		t.Fatalf("Dirs(acme): %v", err)
	}
	dirsB, err := tc.Dirs("globex")
	if err != nil {
		t.Fatalf("Dirs(globex): %v", err)
	}
	if dirsA.GoMod == dirsB.GoMod || dirsA.Npm == dirsB.Npm || dirsA.Pnpm == dirsB.Pnpm {
		t.Fatalf("tenants share a cache lower dir: %+v vs %+v", dirsA, dirsB)
	}

	mountsA := lpA.sess.Mounts(wsA)
	mountsB := lpB.sess.Mounts(wsB)

	gomodA := overlayMountNamed(t, mountsA, dirsA.GoMod)
	gomodB := overlayMountNamed(t, mountsB, dirsB.GoMod)

	if gomodA.Source == gomodB.Source {
		t.Fatalf("tenants' gomod overlay lower dirs are the same: %s", gomodA.Source)
	}
	if gomodA.Upper == gomodB.Upper || gomodA.Work == gomodB.Work {
		t.Fatalf("tenants' overlay upper/work dirs collide: %+v vs %+v", gomodA, gomodB)
	}
	if gomodA.Target != gomodA.Source {
		t.Fatalf("overlay mount target %s != source %s", gomodA.Target, gomodA.Source)
	}

	segmentA := cacheSegment("acme")
	rendered := renderedMounts(lpB, wsB)
	if strings.Contains(rendered, segmentA) {
		t.Fatalf("tenant globex's pod spec names tenant acme's cache segment %q:\n%s", segmentA, rendered)
	}
	if strings.Contains(rendered, dirsA.GoMod) {
		t.Fatalf("tenant globex's pod spec names tenant acme's gomod dir %q:\n%s", dirsA.GoMod, rendered)
	}
}

// TestCacheMountProbeRefusedTurnsOffTenantOverlays proves goal
// the-warm-cache-mounts-on-the-nodes-podman, item 2: a refused probe
// runs once, logs "cache mount off" once, and from then on every
// lease's pod gets no tenant cache overlay (and no tenant cache env)
// while still keeping its GOCACHE mount — never held back for it.
func TestCacheMountProbeRefusedTurnsOffTenantOverlays(t *testing.T) {
	rt := testRuntime(t)
	var logs []string
	rt.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
		t.Logf(format, args...)
	}
	calls := 0
	rt.CacheMountProbe = func(context.Context) (bool, error) {
		calls++
		return true, nil
	}

	rt.probeCacheMountOnce(context.Background())
	rt.probeCacheMountOnce(context.Background())
	rt.probeCacheMountOnce(context.Background())

	if calls != 1 {
		t.Fatalf("CacheMountProbe called %d times, want exactly once", calls)
	}
	if !rt.cacheMountIsOff() {
		t.Fatal("cacheMountIsOff() is false after a refused probe")
	}
	var off int
	for _, l := range logs {
		if l == "nodev1: cache mount off" {
			off++
		}
	}
	if off != 1 {
		t.Fatalf(`"cache mount off" logged %d times, want exactly once; logs: %v`, off, logs)
	}

	ws := t.TempDir()
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	for i, leaseID := range []string{"lease-1", "lease-2"} {
		lp := openTestLeasePod(t, rt, "acme", leaseID, ws)
		for _, m := range lp.sess.Mounts(ws) {
			if m.Overlay {
				t.Fatalf("lease %d still got an overlay mount with cache mount off: %+v", i, m)
			}
		}
		var gocache string
		for _, e := range lp.sess.Env(nil) {
			if strings.HasPrefix(e, "GOMODCACHE=") || strings.HasPrefix(e, "npm_config_cache=") || strings.HasPrefix(e, "pnpm_config_store_dir=") {
				t.Fatalf("lease %d still got tenant cache env with cache mount off: %q", i, e)
			}
			if strings.HasPrefix(e, "GOCACHE=") {
				gocache = e
			}
		}
		if gocache == "" {
			t.Fatalf("lease %d lost its GOCACHE mount when cache mount went off", i)
		}
	}
}

// TestCacheMountProbeErrorLeavesCacheMountOn proves a probe error that
// is not the overlay refusal changes nothing: it is logged, run once,
// and tenant cache overlays stay on for leases to attempt themselves.
func TestCacheMountProbeErrorLeavesCacheMountOn(t *testing.T) {
	rt := testRuntime(t)
	calls := 0
	rt.CacheMountProbe = func(context.Context) (bool, error) {
		calls++
		return false, fmt.Errorf("boom")
	}

	rt.probeCacheMountOnce(context.Background())
	rt.probeCacheMountOnce(context.Background())

	if calls != 1 {
		t.Fatalf("CacheMountProbe called %d times, want exactly once", calls)
	}
	if rt.cacheMountIsOff() {
		t.Fatal("cacheMountIsOff() is true after a probe error that was not a refusal")
	}
}

func TestBuildCacheMountSharedAcrossTenants(t *testing.T) {
	rt := testRuntime(t)
	wsA := t.TempDir()
	wsB := t.TempDir()
	for _, ws := range []string{wsA, wsB} {
		if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
	}

	lpA := openTestLeasePod(t, rt, "acme", "lease-a", wsA)
	lpB := openTestLeasePod(t, rt, "globex", "lease-b", wsB)

	wantGocache := filepath.Join(rt.WorkRoot, "build-cache")

	var mountA, mountB *sandbox.Mount
	for _, m := range lpA.sess.Mounts(wsA) {
		if m.Source == wantGocache {
			mountA = &m
		}
	}
	for _, m := range lpB.sess.Mounts(wsB) {
		if m.Source == wantGocache {
			mountB = &m
		}
	}
	if mountA == nil {
		t.Fatalf("tenant acme has no build cache mount at %s", wantGocache)
	}
	if mountB == nil {
		t.Fatalf("tenant globex has no build cache mount at %s", wantGocache)
	}
	if mountA.Overlay || mountB.Overlay {
		t.Fatalf("build cache mount is an overlay, want a plain read-write bind: %+v / %+v", mountA, mountB)
	}
	if mountA.Source != mountB.Source {
		t.Fatalf("build cache mount source differs across tenants: %s vs %s", mountA.Source, mountB.Source)
	}
	if mountA.Target != mountA.Source || mountB.Target != mountB.Source {
		t.Fatalf("build cache mount target != source: %+v / %+v", mountA, mountB)
	}

	var envA, envB string
	for _, e := range lpA.sess.Env(nil) {
		if strings.HasPrefix(e, "GOCACHE=") {
			envA = e
		}
	}
	for _, e := range lpB.sess.Env(nil) {
		if strings.HasPrefix(e, "GOCACHE=") {
			envB = e
		}
	}
	if envA == "" || envB == "" {
		t.Fatalf("GOCACHE not set for both tenants: %q / %q", envA, envB)
	}
	if envA != envB {
		t.Fatalf("GOCACHE differs across tenants: %q vs %q", envA, envB)
	}
}
