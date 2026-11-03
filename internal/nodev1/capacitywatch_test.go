package nodev1

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

func TestReadCapacityConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")

	if _, ok, err := ReadCapacityConfig(path); err != nil || ok {
		t.Fatalf("a missing file: ok %v, err %v, want false, nil", ok, err)
	}

	if err := os.WriteFile(path, []byte("sandbox:\n  mode: on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadCapacityConfig(path); err != nil || ok {
		t.Fatalf("a file with no v1.capacity: ok %v, err %v, want false, nil", ok, err)
	}

	if err := os.WriteFile(path, []byte("v1:\n  capacity:\n    cpus: 64\n    mem_gb: 128\n    slots: 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, ok, err := ReadCapacityConfig(path)
	if err != nil || !ok {
		t.Fatalf("a file naming v1.capacity: ok %v, err %v, want true, nil", ok, err)
	}
	if c != (meshapi.Capacity{CPUs: 64, MemGB: 128, Slots: 4}) {
		t.Fatalf("read %+v", c)
	}
}

// WatchCapacity applies a change from its read function on its poll,
// with no reload, and keeps polling once it has.
func TestWatchCapacityAppliesOnEachPoll(t *testing.T) {
	r := &Runtime{}
	r.Scheduler = scheduler.New(scheduler.Capacity{CPUs: 48})

	var current meshapi.Capacity = meshapi.Capacity{CPUs: 48}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.WatchCapacity(ctx, func() (meshapi.Capacity, bool, error) { return current, true, nil }, 10*time.Millisecond, nil)
		close(done)
	}()

	current = meshapi.Capacity{CPUs: 64}
	deadline := time.Now().Add(2 * time.Second)
	for r.capacityTotal().CPUs != 64 {
		if time.Now().After(deadline) {
			t.Fatalf("capacity never reached 64: %+v", r.capacityTotal())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.Scheduler.Total().CPUs; got != 64 {
		t.Fatalf("the scheduler's total: %g, want 64", got)
	}
	cancel()
	<-done
}
