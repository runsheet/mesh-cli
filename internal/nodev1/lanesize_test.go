package nodev1

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
	"projectmesh.io/mesh-cli/internal/validate"
)

// laneRooms runs g's integration suite and answers, for each of its
// shards' containers, the limits it ran with, and every lane hold the
// scheduler listed while a shard ran, by owner.
func laneRooms(t *testing.T, g *shardRig) (limits []sandbox.Limits, holds map[string]scheduler.Ask) {
	t.Helper()
	var mu sync.Mutex
	holds = map[string]scheduler.Ask{}
	g.f.Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		if !strings.Contains(spec.Name, "-exec-") {
			return sandbox.Exit{}, nil
		}
		mu.Lock()
		limits = append(limits, spec.Limits)
		for _, h := range g.rt.Scheduler.Leases() {
			holds[h.Owner] = h.Ask
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return sandbox.Exit{}, nil
	}
	if _, last := g.run(t, map[string]any{"suite": "integration"}); last["exit"] != float64(0) {
		t.Fatalf("the run answered %v", last)
	}
	return limits, holds
}

// A lane takes the room its suite asks for, not its lease's (goal
// test-runs-get-the-room-their-suite-needs): inside a 2-CPU build lease,
// a suite asking for 8 CPUs and 12 GB runs every shard — the first in
// the lease's own pod, the other in a lane beside it — limited to 8 CPUs
// and 12 GB, and each holds exactly that of the node's scheduler, given
// back when the run ends.
func TestALaneInATwoCPULeaseGetsTheEightItsSuiteAsksFor(t *testing.T) {
	const yml = "suites:\n  - name: integration\n    kind: integration\n    command: go test ./e2e\n    shards: 2\n    cpus: 8\n    memory: 12g\n"
	g := newShardRigFor(t, yml, 0, scheduler.Capacity{CPUs: 32, MemGB: 64, Slots: 3}, func(b *meshapi.Brief) {
		b.Kind = "build"
		b.Resources = meshapi.Resources{CPUs: 2, MemGB: 4}
	})
	limits, holds := laneRooms(t, g)
	if len(limits) != 2 {
		t.Fatalf("%d shards ran, want 2", len(limits))
	}
	for _, l := range limits {
		if l.CPUs != 8 || l.Memory != "12288m" {
			t.Fatalf("a shard ran limited to %+v, want its suite's 8 cpus and 12g", l)
		}
	}
	for _, owner := range []string{"lease-1/lane-integration-0", "lease-1/lane-integration-1"} {
		if a, ok := holds[owner]; !ok || a.CPUs != 8 || a.MemGB != 12 {
			t.Fatalf("%s holds %+v of the scheduler (held: %v), want what its pod is limited to: 8 cpus, 12 GB", owner, a, ok)
		}
	}
	if free := g.rt.Scheduler.Free(); free.CPUs != 32 || free.MemGB != 64 {
		t.Fatalf("the lanes' room was not given back: %+v", free)
	}
}

// A suite that names no size runs on the node's small default (the
// sandbox's own cpus and memory, 2 and 6g here), never the kind's full
// allowance (the brief's 4 CPUs and 8 GB): its four lanes hold 8 CPUs of
// the scheduler, not 16.
func TestASuiteNamingNoSizeGetsTheNodesDefaultNotTheKindsAllowance(t *testing.T) {
	g := newShardRig(t, shardedYML, 0, scheduler.Capacity{CPUs: 32, MemGB: 64, Slots: 3})
	var held float64
	for _, h := range g.rt.Scheduler.Leases() {
		held += h.Ask.CPUs
	}
	// admission takes the first shard alone (validate.AdmitSizes), and
	// its 2 are within the lease's own hold (admitLanes)
	if held != 0 {
		t.Fatalf("a suite naming no size holds %g cpus at admission, want 0 (its first shard within the lease's pod)", held)
	}
	limits, holds := laneRooms(t, g)
	if len(limits) != 4 {
		t.Fatalf("%d shards ran, want 4", len(limits))
	}
	for _, l := range limits {
		if l.CPUs != 2 || l.Memory != "6144m" {
			t.Fatalf("a shard ran limited to %+v, want the node's default 2 cpus and 6g", l)
		}
	}
	for owner, a := range holds {
		if strings.HasSuffix(owner, "integration-0") {
			continue // runs on the lease's own hold
		}
		if a.CPUs != 2 || a.MemGB != 6 {
			t.Fatalf("%s holds %+v, want 2 cpus and 6 GB", owner, a)
		}
	}
}

// A lane asking for more than the node could ever give it beside its
// lease is lowered to what could come free, never left waiting for room
// that cannot.
func TestALanesAskIsLoweredToWhatTheNodeCouldGive(t *testing.T) {
	g := newShardRig(t, shardedYML, 0, scheduler.Capacity{CPUs: 12, MemGB: 64})
	room := g.rt.laneRoom(g.b, "integration-1", validate.Resources{CPUs: 64, Memory: "8g"})
	if room.CPUs != 8 || room.MemGB != 8 {
		t.Fatalf("a 64-cpu ask on a 12-cpu node beside a 4-cpu lease is %+v, want 8 cpus and its 8 GB", room)
	}
}

// A lane refused room never runs wider than the CPUs it has (goal
// every-hung-test-is-found-and-fixed-by-its-cause, 2026-11-16). On a
// node with no room left, a validation's first shard — the lane in the
// lease's own pod — does not wait: it runs under the lease's own hold,
// its step limited to the 4 CPUs its suite asks for, no more than the
// lease's 4, with GOMAXPROCS, OMP_NUM_THREADS and MAKEFLAGS told the
// same, so go test's -p and -parallel follow it. It runs alone there:
// each other shard waits for room of its own, opening its lane only when
// the node has it, never falling back into the lease's pod beside it.
func TestALaneRefusedRoomRunsOnItsOwnRoomAndTheOthersWait(t *testing.T) {
	g := newShardRig(t, sizedYML, 0, scheduler.Capacity{CPUs: 16, MemGB: 64})
	// the node fills up before the run: the lease's early holds go back
	// and another lease takes every CPU
	g.lp.releaseEagerLanes()
	other, err := g.rt.Scheduler.TryTake(scheduler.Ask{Owner: "another-lease", CPUs: 16, MemGB: 64})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logged []string
	g.rt.Logf = func(format string, args ...any) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	type ran struct {
		inLease bool
		limits  sandbox.Limits
		at      time.Time
	}
	var runs []ran
	g.f.Agent = func(_ context.Context, spec sandbox.AgentSpec) (sandbox.Exit, error) {
		if !strings.Contains(spec.Name, "-exec-") {
			return sandbox.Exit{}, nil
		}
		mu.Lock()
		runs = append(runs, ran{inLease: strings.HasPrefix(spec.Name, g.lp.name+"-exec-"), limits: spec.Limits, at: time.Now()})
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return sandbox.Exit{}, nil
	}
	var freed time.Time
	go func() {
		time.Sleep(400 * time.Millisecond)
		mu.Lock()
		freed = time.Now()
		mu.Unlock()
		other.Release()
	}()
	if _, last := g.run(t, map[string]any{"suite": "integration"}); last["exit"] != float64(0) {
		t.Fatalf("the run answered %v", last)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != 4 {
		t.Fatalf("%d shards ran, want 4", len(runs))
	}
	inLease := 0
	for _, r := range runs {
		if !r.inLease {
			if r.at.Before(freed) {
				t.Fatalf("a shard opened its lane at %s, before the node had room for it at %s: it did not wait", r.at, freed)
			}
			continue
		}
		inLease++
		if !r.at.Before(freed) {
			t.Fatal("the lease's own shard waited for room its lease already holds")
		}
		if r.limits.CPUs != 4 || r.limits.Memory != "8192m" {
			t.Fatalf("the shard in the lease's pod ran limited to %+v, want its suite's 4 cpus and 8g", r.limits)
		}
		if env := sandbox.LimitEnv(r.limits); !slices.Contains(env, "GOMAXPROCS=4") || !slices.Contains(env, "OMP_NUM_THREADS=4") {
			t.Fatalf("the shard in the lease's pod is told %v, want GOMAXPROCS and OMP_NUM_THREADS at its 4 cpus", env)
		}
	}
	if inLease != 1 {
		t.Fatalf("%d shards ran in the lease's own pod, want the first alone", inLease)
	}
	said := strings.Join(logged, "\n")
	// admission took the first lane as what it needs beyond the lease's
	// own hold, nothing here: readmitted as that, it runs on the lease's
	// hold, as a first lane with no room does
	if !strings.Contains(said, "lane integration-0 readmitted: held 0 cpus, 0 GB again") {
		t.Fatalf("the node's log did not say the first lane ran under the lease's hold:\n%s", said)
	}
	if free := g.rt.Scheduler.Free(); free.CPUs != 16 || free.MemGB != 64 {
		t.Fatalf("the lanes' room was not given back: %+v", free)
	}
}

// laneCacheRig is a lease's pod on a node with a tenant cache and a
// build cache, and what closes the lease.
func laneCacheRig(t *testing.T) (rt *Runtime, lp *leasePod, ws string, end func()) {
	t.Helper()
	rt = testRuntime(t)
	rt.TenantCache = NewTenantCache(rt.WorkRoot, CacheConfig{})
	rt.BuildCache = filepath.Join(rt.WorkRoot, "go-build")
	ws = t.TempDir()
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	b := &meshapi.Brief{JobID: "job-1", LeaseID: "lease-1", LeaseN: 1, Kind: "validate",
		Product: "runsheet", Tenant: "acme", Resources: meshapi.Resources{CPUs: 1, MemGB: 1}}
	var closers []func() error
	var once sync.Once
	end = func() {
		once.Do(func() {
			for _, c := range closers {
				c()
			}
		})
	}
	t.Cleanup(end)
	lp, err := rt.openLeasePod(context.Background(), b, ws, nil, func(_ string, c func() error) { closers = append(closers, c) })
	if err != nil {
		t.Fatal(err)
	}
	return rt, lp, ws, end
}

// envOf is name's value in env, "" when it is not set.
func envOf(env []string, name string) string {
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == name {
			return v
		}
	}
	return ""
}

// A lane starts from the lease's warm cache (goal
// a-run-suite-lane-starts-from-the-leases-warm-cache): opened beside a
// lease on a node with a tenant cache and a build cache, its pod mounts
// the tenant's GOMODCACHE overlay and the node's shared GOCACHE at the
// paths the lease's pod mounts them, and names them in the same
// variables; its overlay's upper and work dirs are its own, under the
// lease's directory — never the lease's, never another lane's.
func TestALaneMountsTheLeasesWarmCache(t *testing.T) {
	rt, lp, ws, _ := laneCacheRig(t)
	dirs, err := rt.TenantCache.Dirs("acme")
	if err != nil {
		t.Fatal(err)
	}
	leaseMounts := lp.sess.Mounts(ws)
	leaseGoMod := overlayMountNamed(t, leaseMounts, dirs.GoMod)
	leaseEnv := lp.sess.Env(nil)

	var gomods []sandbox.Mount
	for _, name := range []string{"integration-1", "integration-2"} {
		lane, release, err := lp.lanes(context.Background(), name, validate.LaneSize{Services: true})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		mounts := lane.sess.Mounts(ws)
		for _, source := range []string{dirs.GoMod, dirs.Npm, dirs.Pnpm} {
			m := overlayMountNamed(t, mounts, source)
			if m.Target != source {
				t.Fatalf("lane %s mounts %s at %s, not where the lease's pod does", name, source, m.Target)
			}
			if !strings.HasPrefix(m.Upper, rt.leaseDir("lease-1")+string(filepath.Separator)) || !strings.HasPrefix(m.Work, rt.leaseDir("lease-1")+string(filepath.Separator)) {
				t.Fatalf("lane %s's overlay dirs %s, %s are not under its lease's directory", name, m.Upper, m.Work)
			}
			for _, dir := range []string{m.Upper, m.Work} {
				if st, err := os.Stat(dir); err != nil || !st.IsDir() {
					t.Fatalf("lane %s's overlay dir %s was not made: %v", name, dir, err)
				}
			}
		}
		gomods = append(gomods, overlayMountNamed(t, mounts, dirs.GoMod))
		if !slices.ContainsFunc(mounts, func(m sandbox.Mount) bool {
			return !m.Overlay && m.Source == rt.BuildCache && m.Target == rt.BuildCache && !m.ReadOnly
		}) {
			t.Fatalf("lane %s does not mount the node's build cache %s: %+v", name, rt.BuildCache, mounts)
		}
		env := lane.sess.Env(nil)
		for _, v := range []string{"GOMODCACHE", "GOCACHE", "npm_config_cache", "pnpm_config_store_dir"} {
			if got, want := envOf(env, v), envOf(leaseEnv, v); want == "" || got != want {
				t.Fatalf("lane %s has %s=%q, the lease's pod %q", name, v, got, want)
			}
		}
		if envOf(env, "GOMODCACHE") != dirs.GoMod || envOf(env, "GOCACHE") != rt.BuildCache {
			t.Fatalf("lane %s's GOMODCACHE and GOCACHE are not the mounts': %v", name, env)
		}
	}
	uppers := []string{leaseGoMod.Upper, gomods[0].Upper, gomods[1].Upper}
	works := []string{leaseGoMod.Work, gomods[0].Work, gomods[1].Work}
	for _, dirs := range [][]string{uppers, works} {
		if dirs[0] == dirs[1] || dirs[0] == dirs[2] || dirs[1] == dirs[2] {
			t.Fatalf("the lease and its two lanes share an overlay dir: %v", dirs)
		}
	}
}

// A lease whose pod runs without its tenant cache (the runtime refused
// the overlay) has lanes without it too, on the build cache alone.
func TestALaneOfALeaseWithoutItsTenantCacheHasNone(t *testing.T) {
	rt, lp, ws, _ := laneCacheRig(t)
	lp.sess.DropMounts(tenantCacheMount)
	lp.sess.DropEnv(tenantCacheEnv...)
	lane, release, err := lp.lanes(context.Background(), "integration-1", validate.LaneSize{Services: true})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, m := range lane.sess.Mounts(ws) {
		if m.Overlay {
			t.Fatalf("the lane mounts an overlay its lease's pod does not: %+v", m)
		}
	}
	env := lane.sess.Env(nil)
	if envOf(env, "GOMODCACHE") != "" || envOf(env, "GOCACHE") != rt.BuildCache {
		t.Fatalf("the lane's cache variables are not the lease's: %v", env)
	}
}

// A lane's end removes its own overlay dirs, and the lease's end removes
// every lane's with its own: a lane still open then (the run cut short)
// leaves nothing under the lease's directory.
func TestTheLeasesEndRemovesEveryLanesOverlayDirs(t *testing.T) {
	rt, lp, ws, end := laneCacheRig(t)
	dirs, err := rt.TenantCache.Dirs("acme")
	if err != nil {
		t.Fatal(err)
	}
	var made []string
	var releases []func()
	for _, name := range []string{"integration-1", "integration-2", "integration-3"} {
		lane, release, err := lp.lanes(context.Background(), name, validate.LaneSize{Services: true})
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
		m := overlayMountNamed(t, lane.sess.Mounts(ws), dirs.GoMod)
		// what a lane's `go mod download` leaves in its upper: read-only
		// directories a plain RemoveAll cannot remove
		mod := filepath.Join(m.Upper, "example.com", "dep@v1.0.0")
		if err := os.MkdirAll(mod, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mod, "dep.go"), []byte("package dep"), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(mod, 0o555); err != nil {
			t.Fatal(err)
		}
		made = append(made, m.Upper, m.Work)
	}
	gone := func(dir string) bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err)
	}
	// the third lane ends on its own
	releases[2]()
	for _, dir := range made[4:] {
		if !gone(dir) {
			t.Fatalf("a lane's end left its overlay dir %s", dir)
		}
	}
	for _, dir := range made[:4] {
		if gone(dir) {
			t.Fatalf("a lane's end removed another lane's overlay dir %s", dir)
		}
	}
	// the lease ends with two lanes still open
	end()
	for _, dir := range made {
		if !gone(dir) {
			t.Fatalf("the lease's end left a lane's overlay dir %s", dir)
		}
	}
	if !gone(filepath.Join(rt.leaseDir("lease-1"), lanesDirName)) {
		t.Fatal("the lease's end left its lanes' directory")
	}
}
