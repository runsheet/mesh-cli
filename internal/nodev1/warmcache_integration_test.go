package nodev1

// warmcache_integration_test.go proves, end to end, what cache.go and
// cachemount.go (goal a-v1-lease-starts-from-a-warm-cache) exist for:
// TestTwoSequentialLeasesReuseModuleCache runs a real `go build` under
// two sequential leases of one tenant and shows the second needs no
// network for a module the first one fetched, because promoteLeaseCache
// (lease.go) merged the first lease's overlay writes into the tenant's
// shared cache before the second started. TestCrossTenantCacheIsolation
// shows a second tenant's lease never mounts, names or can read the
// first tenant's cache directory.
//
// Neither test mounts a real OverlayFS: that needs a container runtime
// (none is on this machine, cachemount_test.go's testRuntime included),
// and the overlay mount is only ever applied inside a lease's pod, never
// on the node's own host filesystem. Each lease's write destination
// under a real overlay is its own upper dir (overlayMount's Upper); a
// lease with nothing yet in its upper and a non-empty lower reads
// through to the lower unchanged — exactly dirs.GoMod, the tenant's
// shared cache, since overlayMount's Target equals its Source. So a
// cold lease's GOMODCACHE is simulated as its own upper dir (where its
// downloads would really land) and a lease starting warm, with nothing
// of its own yet written, as the tenant's shared lower dir directly
// (what it would transparently see through an empty overlay upper).

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// makeTreeWritable chmods every file and dir under root to 0700, so a
// read-only Go module cache tree (go mod download's own tamper-guard)
// can still be removed by t.TempDir's cleanup.
func makeTreeWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		_ = os.Chmod(path, 0o700)
		return nil
	})
}

// buildModuleDir writes a minimal module under dir that imports
// github.com/pkg/errors v0.9.1 — a real, small, dependency-free public
// module, so a cold GOMODCACHE prints Go's own "go: downloading
// github.com/pkg/errors v0.9.1" and a warm one does not.
func buildModuleDir(t *testing.T, dir string) {
	t.Helper()
	goMod := "module example.com/warmcache\n\ngo 1.21\n\nrequire github.com/pkg/errors v0.9.1\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("go.mod: %v", err)
	}
	main := "package main\n\nimport \"github.com/pkg/errors\"\n\nfunc main() { _ = errors.New(\"x\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(main), 0o600); err != nil {
		t.Fatalf("main.go: %v", err)
	}
}

// goBuildWithCache runs `go build` of the module at dir with GOMODCACHE
// set to gomodcache, answering its combined output. proxyOff leaves
// GOPROXY at its default (reaching the real module proxy) when false,
// or forces GOPROXY=off (no network at all) when true.
func goBuildWithCache(t *testing.T, dir, gomodcache string, proxyOff bool) (string, error) {
	t.Helper()
	out := filepath.Join(dir, "out")
	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", out, ".")
	cmd.Dir = dir
	env := append(os.Environ(),
		"GOMODCACHE="+gomodcache,
		"GOFLAGS=-mod=mod",
		"GOSUMDB=off",
	)
	if proxyOff {
		env = append(env, "GOPROXY=off")
	}
	cmd.Env = env
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func TestTwoSequentialLeasesReuseModuleCache(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}

	root := t.TempDir()
	// go mod download leaves extracted module trees read-only (its usual
	// tamper-guard); make everything writable again before t.TempDir's
	// own cleanup tries to remove it, or that cleanup fails with
	// "permission denied". Registered after TempDir, so it runs first
	// (t.Cleanup is LIFO).
	t.Cleanup(func() { makeTreeWritable(root) })
	tc := NewTenantCache(root, CacheConfig{})
	rt := &Runtime{WorkRoot: root, TenantCache: tc, Logf: t.Logf}
	const tenant = "acme"

	dirs, err := tc.Dirs(tenant)
	if err != nil {
		t.Fatalf("Dirs(%s): %v", tenant, err)
	}

	// Lease 1: cold. Its overlay upper dir (where a real write under an
	// overlay mount would land, since the tenant's shared lower starts
	// empty) is its GOMODCACHE.
	leaseDir1 := rt.leaseDir("lease-1")
	mount1, err := rt.overlayMount(leaseDir1, "gomod", dirs.GoMod)
	if err != nil {
		t.Fatalf("overlayMount(lease-1): %v", err)
	}
	modDir1 := t.TempDir()
	buildModuleDir(t, modDir1)

	out1, err := goBuildWithCache(t, modDir1, mount1.Upper, false)
	if err != nil {
		t.Fatalf("lease 1 build: %v\n%s", err, out1)
	}
	if !strings.Contains(out1, "go: downloading github.com/pkg/errors") {
		t.Fatalf("lease 1 (cold cache) should have downloaded the module, got:\n%s", out1)
	}

	// The lease ends: promote its overlay writes into the tenant's
	// shared cache, as work() does for a non-actor lease (lease.go,
	// promoteLeaseCache, called when a == nil).
	rt.promoteLeaseCache(&meshapi.Brief{Tenant: tenant, LeaseID: "lease-1"}, leaseDir1)

	if _, err := os.Stat(filepath.Join(dirs.GoMod, "cache", "download", "github.com", "pkg", "errors", "@v", "v0.9.1.zip")); err != nil {
		t.Fatalf("promote did not land the module in the tenant's shared cache: %v", err)
	}

	// Lease 2: starts from the warm tenant cache. Its own overlay upper
	// is still empty, so what it would see through the overlay is the
	// tenant's shared lower unchanged — dirs.GoMod itself (overlayMount
	// mounts Target == Source there).
	leaseDir2 := rt.leaseDir("lease-2")
	if _, err := rt.overlayMount(leaseDir2, "gomod", dirs.GoMod); err != nil {
		t.Fatalf("overlayMount(lease-2): %v", err)
	}
	modDir2 := t.TempDir()
	buildModuleDir(t, modDir2)

	// GOPROXY=off: lease 2 cannot reach the network at all, proving
	// whatever it builds with comes from the tenant's warm cache alone.
	out2, err := goBuildWithCache(t, modDir2, dirs.GoMod, true)
	if err != nil {
		t.Fatalf("lease 2 build (expected to succeed from the warm cache, GOPROXY=off): %v\n%s", err, out2)
	}
	if strings.Contains(out2, "go: downloading") {
		t.Fatalf("lease 2 should reuse the tenant's warm cache with no download, got:\n%s", out2)
	}
}

func TestCrossTenantCacheIsolation(t *testing.T) {
	root := t.TempDir()
	tc := NewTenantCache(root, CacheConfig{})
	rt := &Runtime{WorkRoot: root, TenantCache: tc, Logf: t.Logf}

	dirsA, err := tc.Dirs("acme")
	if err != nil {
		t.Fatalf("Dirs(acme): %v", err)
	}
	dirsB, err := tc.Dirs("globex")
	if err != nil {
		t.Fatalf("Dirs(globex): %v", err)
	}

	// Tenant A's lease writes a sentinel into its own cache, as a real
	// build would write a downloaded module there.
	leaseDirA := rt.leaseDir("lease-a")
	mountA, err := rt.overlayMount(leaseDirA, "gomod", dirsA.GoMod)
	if err != nil {
		t.Fatalf("overlayMount(acme): %v", err)
	}
	const secret = "tenant-acme-only"
	sentinelRel := filepath.Join("cache", "download", "sentinel.txt")
	sentinelPath := filepath.Join(mountA.Upper, sentinelRel)
	if err := os.MkdirAll(filepath.Dir(sentinelPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(sentinelPath, []byte(secret), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	rt.promoteLeaseCache(&meshapi.Brief{Tenant: "acme", LeaseID: "lease-a"}, leaseDirA)

	// Promotion did land it in tenant acme's own shared cache.
	promoted := filepath.Join(dirsA.GoMod, sentinelRel)
	if got, err := os.ReadFile(promoted); err != nil || string(got) != secret {
		t.Fatalf("sentinel not promoted into acme's cache: %v, %q", err, got)
	}

	// Tenant globex's lease mounts its own, separate overlay.
	leaseDirB := rt.leaseDir("lease-b")
	mountB, err := rt.overlayMount(leaseDirB, "gomod", dirsB.GoMod)
	if err != nil {
		t.Fatalf("overlayMount(globex): %v", err)
	}

	if mountB.Source == dirsA.GoMod || mountB.Target == dirsA.GoMod || mountB.Upper == mountA.Upper || mountB.Work == mountA.Work {
		t.Fatalf("tenant globex's overlay names tenant acme's path: %+v vs %+v", mountB, mountA)
	}

	// The read tenant B's pod would actually make — its own overlay
	// mount's path joined with the sentinel's relative path — finds
	// nothing: no mount or env of tenant B's ever names acme's segment
	// or cache dir, so there is no path under globex's cache that
	// resolves to acme's file.
	if _, err := os.ReadFile(filepath.Join(mountB.Source, sentinelRel)); !os.IsNotExist(err) {
		t.Fatalf("tenant globex can read tenant acme's cache entry (err=%v); isolation broken", err)
	}
	if _, err := os.ReadFile(filepath.Join(mountB.Upper, sentinelRel)); !os.IsNotExist(err) {
		t.Fatalf("tenant globex's own overlay upper unexpectedly has acme's entry (err=%v)", err)
	}

	segmentA := cacheSegment("acme")
	for _, p := range []string{mountB.Source, mountB.Target, mountB.Upper, mountB.Work} {
		if strings.Contains(p, segmentA) {
			t.Fatalf("tenant globex's mount path %q names tenant acme's cache segment %q", p, segmentA)
		}
	}

	// A fresh read of tenant acme's dirs from the shared TenantCache
	// still resolves to the same lower, proving the isolation held
	// tenant acme's data intact rather than merely never looking at it.
	dirsAAgain, err := tc.Dirs("acme")
	if err != nil || dirsAAgain.GoMod != dirsA.GoMod {
		t.Fatalf("tenant acme's cache dir moved or vanished: %+v (err=%v)", dirsAAgain, err)
	}
	if got, err := os.ReadFile(promoted); err != nil || string(got) != secret {
		t.Fatalf("tenant acme's own cache entry did not survive: %v, %q", err, got)
	}
}
