package node

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// The disk floor (diskfloor.go): under node.yaml's disk.free_min_gb on
// the work root the node claims nothing of any kind, said once as it
// enters the state and once as it leaves it.

// fakeDisk is a StatfsFunc whose free space the test moves.
type fakeDisk struct {
	mu    sync.Mutex
	free  uint64
	paths []string
}

func (d *fakeDisk) set(free uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.free = free
}

func (d *fakeDisk) statfs(path string) (DiskStat, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paths = append(d.paths, path)
	return DiskStat{Free: d.free, Total: 500 << 30, Device: 1}, nil
}

const gibBytes = uint64(1) << 30

// (a) and (b): with free space under the floor, three ticks over a
// build, an evaluation, a plan, a rebase and a direction claim nothing
// and log one disk_low line; once free space rises the next tick claims
// and logs disk ok.
func TestDiskLowClaimsNothingThenClaimsOnceFreed(t *testing.T) {
	p := takesFake(t)
	loop, logs := p.loop(2)
	disk := &fakeDisk{free: 2 * gibBytes}
	loop.WorkRoot, loop.Statfs = "/work", disk.statfs
	var evaluated atomic.Bool
	loop.Evaluate = func(context.Context, gateway.WorkItem) { evaluated.Store(true) }
	loop.Rebase = func(context.Context, gateway.WorkItem) {}

	for range 3 {
		tick(loop)
	}
	if got := claimCalls(p); len(got) != 0 {
		t.Fatalf("claims under the floor = %v, want none\n%s", got, logs.String())
	}
	if evaluated.Load() {
		t.Error("an evaluation started under the floor")
	}
	if n := strings.Count(logs.String(), "disk_low:"); n != 1 {
		t.Fatalf("%d disk_low lines over three ticks, want 1:\n%s", n, logs.String())
	}
	want := "disk_low: /work 2.0GiB free < floor 10.0GiB; claiming nothing"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("node.log lacks %q:\n%s", want, logs.String())
	}
	if f := loop.DiskFindings(); len(f) != 1 || f[0].Kind != "disk_low" || f[0].Severity != "critical" || f[0].Subject != "/work" || f[0].Bytes != int64(2*gibBytes) {
		t.Errorf("DiskFindings() = %+v, want one critical disk_low on /work", f)
	}
	if disk.paths[0] != "/work" {
		t.Errorf("statfs read %q, want the work root", disk.paths[0])
	}

	disk.set(50 * gibBytes)
	tick(loop)
	for _, name := range []string{"claimWorkItem", "claimPlan", "claimWorkItemRebase", "claimDirectionResearch", "claimWorkItemEvaluation"} {
		if len(p.CallsNamed(name)) == 0 {
			t.Errorf("no %s once free space rose above the floor\n%s", name, logs.String())
		}
	}
	if !evaluated.Load() {
		t.Error("no evaluation started once free space rose above the floor")
	}
	if n := strings.Count(logs.String(), "disk ok: /work 50.0GiB free >= floor 10.0GiB"); n != 1 {
		t.Fatalf("%d disk ok lines, want 1:\n%s", n, logs.String())
	}
	if f := loop.DiskFindings(); len(f) != 0 {
		t.Errorf("DiskFindings() = %+v above the floor, want none", f)
	}
	tick(loop)
	if n := strings.Count(logs.String(), "disk ok:"); n != 1 {
		t.Errorf("%d disk ok lines after another tick, want 1", n)
	}
}

// (a) for a merge batch: under the floor the batch is not claimed; once
// freed it is.
func TestDiskLowClaimsNoBatchValidation(t *testing.T) {
	loop, f, _, logs := batchLoop(t, oneStepYML("true"), 1)
	disk := &fakeDisk{free: 1 * gibBytes}
	loop.WorkRoot, loop.Statfs = "/work", disk.statfs
	tick(loop)
	tick(loop)
	if n := len(f.CallsNamed("claimMergeBatchValidation")); n != 0 {
		t.Fatalf("%d batch claims under the floor, want 0\n%s", n, logs.String())
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c.Name, "claim") {
			t.Errorf("%s under the floor", c.Name)
		}
	}
	if n := strings.Count(logs.String(), "disk_low:"); n != 1 {
		t.Fatalf("%d disk_low lines, want 1:\n%s", n, logs.String())
	}
	disk.set(20 * gibBytes)
	tick(loop)
	if n := len(f.CallsNamed("claimMergeBatchValidation")); n != 1 {
		t.Fatalf("%d batch claims once freed, want 1\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "disk ok:") {
		t.Errorf("no disk ok line:\n%s", logs.String())
	}
}

// A node that starts under the floor says so at its first tick; one
// that starts above it says nothing.
func TestDiskLowSaidAtStart(t *testing.T) {
	p := takesFake(t)
	loop, logs := p.loop(1)
	loop.WorkRoot, loop.Statfs = "/work", (&fakeDisk{free: 50 * gibBytes}).statfs
	tick(loop)
	if strings.Contains(logs.String(), "disk") {
		t.Errorf("a node above the floor said something of the disk:\n%s", logs.String())
	}

	p = takesFake(t)
	loop, logs = p.loop(1)
	loop.WorkRoot, loop.Statfs = "/work", (&fakeDisk{free: 0}).statfs
	tick(loop)
	if !strings.Contains(logs.String(), "disk_low: /work 0.0GiB free < floor 10.0GiB; claiming nothing") {
		t.Errorf("a node starting under the floor did not say so:\n%s", logs.String())
	}
}

// (c) disk.free_min_gb parses, defaults when absent, refuses a negative,
// and reloads: the Reloader carries it and the loop logs the change and
// judges by it at that tick.
func TestDiskLowConfigParsesDefaultsAndReloads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Disk.Effective(); got.FreeMinGB != DefaultDiskFreeMinGB || got.Path != "" {
		t.Fatalf("disk left out = %+v, want free_min_gb %v and the work root", got, DefaultDiskFreeMinGB)
	}

	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\ndisk: {free_min_gb: 25, path: /srv/mesh}\n")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Disk.FreeMinGB != 25 || cfg.Disk.Path != "/srv/mesh" {
		t.Fatalf("disk = %+v, want free_min_gb 25 on /srv/mesh", cfg.Disk)
	}
	if got := cfg.Disk.FloorBytes(); got != 25*gibBytes {
		t.Errorf("FloorBytes() = %d, want %d", got, 25*gibBytes)
	}

	writeNodeYAML(t, "slug: mini\nname: mini\ncapacity: 1\ndisk: {free_min_gb: -1}\n")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "disk.free_min_gb") {
		t.Fatalf("a negative floor: err = %v, want it to name disk.free_min_gb", err)
	}

	f := seed(t)
	gw := gateway.New(f.URL(), "rs_test")
	base := &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", Capacity: Fixed(1)}
	reload := Reloader(gw, base, scopesOf(base), func() (*Config, error) {
		return &Config{OrgLogin: "acme", OrgID: orgID, Slug: "n", Capacity: Fixed(1), Disk: DiskConfig{FreeMinGB: 20}}, nil
	}, func(context.Context, Scope) error { return nil })
	got, _ := reload(context.Background())
	if got.Disk == nil || got.Disk.FreeMinGB != 20 {
		t.Fatalf("Reloaded.Disk = %+v, want free_min_gb 20", got.Disk)
	}

	// the loop applies it: 15 GiB free is above the default 10 and
	// under the reloaded 20
	p := takesFake(t)
	loop, logs := p.loop(1)
	loop.WorkRoot, loop.Statfs = "/work", (&fakeDisk{free: 15 * gibBytes}).statfs
	tick(loop)
	if strings.Contains(logs.String(), "disk_low:") {
		t.Fatalf("disk_low at 15GiB under the default floor:\n%s", logs.String())
	}
	loop.Reload = func(context.Context) (Reloaded, error) { return Reloaded{Disk: &DiskConfig{FreeMinGB: 20}}, nil }
	tick(loop)
	out := logs.String()
	for _, want := range []string{"disk: free_min_gb 10 → 20", "disk_low: /work 15.0GiB free < floor 20.0GiB; claiming nothing"} {
		if !strings.Contains(out, want) {
			t.Errorf("node.log lacks %q:\n%s", want, out)
		}
	}
	if th := loop.DiskThresholds(); th.FreeMinGB != 20 {
		t.Errorf("DiskThresholds() = %+v, want free_min_gb 20", th)
	}
}
