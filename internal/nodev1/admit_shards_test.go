package nodev1

import (
	"context"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sandbox"
	"projectmesh.io/mesh-cli/internal/scheduler"
	"projectmesh.io/mesh-cli/internal/validate"
)

// runsheetLike is runsheet/runsheet's validate.yml in shape (2026-11-23):
// a checks group and a unit group of 4 cpus each, neither running
// services, and an integration suite of four 4-cpu shards, each with
// its own Postgres.
const runsheetLike = `
suites:
  - name: lint
    kind: lint
    group: checks
    command: "true"
    cpus: 4
    memory: 8g
  - name: unit
    kind: unit
    command: "true"
    services: false
    cpus: 4
    memory: 8g
  - name: integration
    kind: integration
    command: "true"
    cpus: 4
    memory: 8g
    shards: 4
`

// A validate lease is admitted for every lane but a sharded suite's
// further shards: a runsheet item validation on dl360 (services 4 cpus,
// 8g) holds 4 + 4 + (4+4) = 16 cpus, not the 40 its four integration
// shards made it ask for when every one was held up front; and a run
// that starts every shard still takes all four, the three past the
// first each waiting for its own room as it opens (holdLane) and giving
// it back to the node when it ends.
func TestAValidationIsAdmittedForOneShardPerSuite(t *testing.T) {
	cfg, err := validate.Parse([]byte(runsheetLike))
	if err != nil {
		t.Fatal(err)
	}
	rt := testRuntime(t)
	rt.Pods.Config.Limits = sandbox.Limits{CPUs: 8, Memory: "16g"}
	rt.Pods.Config.ServicesLimits = sandbox.Limits{CPUs: 4, Memory: "8g"}
	rt.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 80, MemGB: 200, Slots: 8})
	b := &meshapi.Brief{LeaseID: "lease-i", JobID: "job-i", Kind: "runsheet/validate", Resources: meshapi.Resources{CPUs: 4, MemGB: 8}}
	own, err := rt.Scheduler.TryTake(askOf(b, &meshapi.Lease{LeaseID: "lease-i", RenewEveryS: 30}))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Release()
	p := &leasePod{statuses: []sandbox.ServiceStatus{{Name: "postgres", Ready: true}}}
	svc := rt.servicesRoom(p)

	sizes := validate.AdmitSizes(cfg.Steps)
	var names []string
	for _, s := range sizes {
		names = append(names, s.Name)
	}
	if want := []string{"checks", "unit", "integration-0"}; len(names) != len(want) || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("admitted lanes %v, want %v", names, want)
	}
	got, err := rt.admit(context.Background(), b, sizes, svc)
	if err != nil {
		t.Fatal(err)
	}
	if held := 80 - rt.Scheduler.Free().CPUs; held != 16 {
		t.Fatalf("the item validation holds %g cpus with its own, want 16", held)
	}
	for _, a := range got {
		a.l.Release()
	}

	// the full shape, as admission took it before: 40
	all, err := rt.admit(context.Background(), b, validate.LaneSizes(cfg.Steps), svc)
	if err != nil {
		t.Fatal(err)
	}
	if held := 80 - rt.Scheduler.Free().CPUs; held != 40 {
		t.Fatalf("every lane up front holds %g cpus, want 40", held)
	}
	for _, a := range all {
		a.l.Release()
	}

	// a full run: admitted for 16, then shards 1-3 each held as they
	// open, 8 cpus apiece, and each given back to the node at its end
	got, err = rt.admit(context.Background(), b, sizes, svc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, a := range got {
			a.l.Release()
		}
	}()
	full := validate.LaneSizes(cfg.Steps)
	if len(full) != 6 {
		t.Fatalf("a full run has %d lanes, want 6 (checks, unit, 4 shards)", len(full))
	}
	var extra []*laneHold
	for _, s := range full[3:] {
		h, err := rt.holdLane(context.Background(), b, p, s.Name, s, false)
		if err != nil {
			t.Fatalf("shard %s: %v", s.Name, err)
		}
		extra = append(extra, h)
	}
	if held := 80 - rt.Scheduler.Free().CPUs; held != 40 {
		t.Fatalf("a full run holds %g cpus once every shard opened, want 40", held)
	}
	for _, h := range extra {
		h.release()
	}
	if held := 80 - rt.Scheduler.Free().CPUs; held != 16 {
		t.Fatalf("after shards 1-3 ended the lease holds %g cpus, want 16 (their room back to the node)", held)
	}
}
