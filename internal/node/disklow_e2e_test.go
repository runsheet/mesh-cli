package node

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// Goal a-node-low-on-disk-claims-nothing-and-a-missing-tool-is-named,
// item 3 (docs/adr/adr-035): the node's own Loop, run as `mesh node
// start` runs it, against the gatewaytest fake and a real git origin —
// a ready item, an item this node holds in evaluating and a merge batch
// candidate all waiting. Started with the work root's free space under
// node.yaml's disk.free_min_gb (the faked statfs), it logs disk_low once
// and sends no claim of any kind however many ticks pass, nor starts a
// build or an evaluation; once space is freed the next tick says disk ok
// and claims the build, the evaluation and the batch.
func TestDiskLowE2E(t *testing.T) {
	r, f, batch, _, logs := candidateRunner(t, oneStepYML("true"))
	loop, _, _, _ := batchLoopOn(t, r, f, batch, logs, 2)
	// a ready item to build and one of this node's to evaluate, beside
	// the batch
	f.WorkItems[item1]["status"] = "ready"
	f.WorkItems[item2]["status"] = "evaluating"
	f.WorkItems[item2]["nodeId"] = batchNode
	f.WorkItems[item2]["branch"] = "mesh/ship-it-2"

	var built, evaluated atomic.Int32
	loop.Dispatch = func(context.Context, gateway.WorkItem) { built.Add(1) }
	loop.Evaluate = func(context.Context, gateway.WorkItem) { evaluated.Add(1) }
	loop.Interval = 20 * time.Millisecond
	disk := &fakeDisk{free: 3 * gibBytes}
	loop.WorkRoot, loop.Statfs = r.WorkRoot, disk.statfs

	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- loop.Run(ctx) }()
	stop := func() {
		cancel()
		select {
		case <-ran:
		case <-time.After(10 * time.Second):
			t.Fatalf("Run did not return:\n%s", logs.String())
		}
		loop.wg.Wait()
	}
	defer stop()
	reads := func() int {
		disk.mu.Lock()
		defer disk.mu.Unlock()
		return len(disk.paths)
	}
	waitUntil := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !done() {
			if time.Now().After(deadline) {
				t.Fatalf("never %s:\n%s", what, logs.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// under the floor: ten ticks and nothing claimed
	waitUntil("ticked ten times under the floor", func() bool { return reads() >= 10 })
	// every claim mutation the gateway takes, of any kind
	var claims []string
	for _, name := range []string{"claimWorkItem", "claimWorkItemEvaluation", "claimWorkItemRebase", "claimPlan",
		"claimDirectionResearch", "claimMergeBatchValidation", "claimWorkItemMainValidation"} {
		for range f.CallsNamed(name) {
			claims = append(claims, name)
		}
	}
	if len(claims) != 0 {
		t.Fatalf("claims under the floor = %v, want none\n%s", claims, logs.String())
	}
	if built.Load() != 0 || evaluated.Load() != 0 {
		t.Fatalf("under the floor: %d builds and %d evaluations started, want none", built.Load(), evaluated.Load())
	}
	want := "disk_low: " + r.WorkRoot + " 3.0GiB free < floor 10.0GiB; claiming nothing"
	if n := strings.Count(logs.String(), want); n != 1 {
		t.Fatalf("%d %q lines, want 1:\n%s", n, want, logs.String())
	}
	if f := loop.DiskFindings(); len(f) != 1 || f[0].Kind != "disk_low" {
		t.Errorf("DiskFindings() = %+v under the floor, want one disk_low", f)
	}

	// space freed: the build, the evaluation and the batch are claimed
	disk.set(40 * gibBytes)
	waitUntil("claimed once space was freed", func() bool {
		return len(f.CallsNamed("claimWorkItem")) > 0 && len(f.CallsNamed("claimWorkItemEvaluation")) > 0 &&
			len(f.CallsNamed("claimMergeBatchValidation")) > 0 && built.Load() > 0 && evaluated.Load() > 0
	})
	if c := f.CallsNamed("claimWorkItem")[0]; c.Input["aggregateId"] != item1 {
		t.Errorf("claimWorkItem = %v, want %s", c.Input, item1)
	}
	if c := f.CallsNamed("claimMergeBatchValidation")[0]; c.Input["aggregateId"] != batchID {
		t.Errorf("claimMergeBatchValidation = %v, want %s", c.Input, batchID)
	}
	if !strings.Contains(logs.String(), "disk ok: "+r.WorkRoot+" 40.0GiB free >= floor 10.0GiB; claiming again") {
		t.Errorf("node.log lacks the disk ok line:\n%s", logs.String())
	}
	if f := loop.DiskFindings(); len(f) != 0 {
		t.Errorf("DiskFindings() = %+v above the floor, want none", f)
	}
}
