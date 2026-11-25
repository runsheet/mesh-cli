package nodev1

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// learnKind gives kind a learned request of cpus (and a trifling
// amount of memory, never the binding constraint in these tests) by
// ending minMeasuredForLearning leases of it with that peak.
func learnKind(rt *Runtime, kind string, cpus float64) {
	for i := 0; i < minMeasuredForLearning; i++ {
		rt.usage.ended(kind, leasePeaks{read: true, cpu: cpus, mem: 1 << 20})
	}
}

func buildBrief(kind string, n int, limitCPUs float64) *meshapi.Brief {
	return &meshapi.Brief{LeaseID: fmt.Sprintf("%s-%d", kind, n), Kind: "runsheet/" + kind, Resources: meshapi.Resources{CPUs: limitCPUs, MemGB: 1}}
}

// Goal a-job-is-sized-by-request-and-limit, item 3, acceptance check 1:
// with v1.admit_on=requests (the default), a 48-cpu general room admits
// at least 40 of a kind's leases whose learned request is 1 cpu though
// its declared limit is 12 -- the room is sized by requests, never
// limits -- and a lease of a different kind whose own learned request
// is 9 cpus waits once the room left cannot hold it.
func TestAdmitOnRequestsSumsLearnedRequests(t *testing.T) {
	rt := &Runtime{AdmitOn: AdmitOnRequests, Scheduler: scheduler.New(scheduler.Capacity{CPUs: 48, MemGB: 1000})}
	const kind = "build"
	learnKind(rt, kind, 1)
	for i := 0; i < 40; i++ {
		b := buildBrief(kind, i, 12)
		if _, err := rt.Scheduler.TryTake(rt.leaseAsk(b, leaseOf(b.LeaseID))); err != nil {
			t.Fatalf("build %d (request 1, limit 12): %v", i, err)
		}
	}
	if f := rt.Scheduler.Free(); f.CPUs != 8 {
		t.Fatalf("free %g cpus after 40 1-cpu-request builds in a 48-cpu room, want 8 (requests summed, not their 12-cpu limit)", f.CPUs)
	}
	const big = "big"
	learnKind(rt, big, 9)
	ask := buildBrief(big, 0, 12)
	if _, err := rt.Scheduler.TryTake(rt.leaseAsk(ask, leaseOf(ask.LeaseID))); !errors.Is(err, scheduler.ErrNoRoom) {
		t.Fatalf("a 9-cpu-request build with 8 cpus free: err %v, want no room", err)
	}
}

// Goal a-job-is-sized-by-request-and-limit, item 3, acceptance check 2:
// the same fixture, kind for kind, with v1.admit_on=declared and every
// build's declared size at 4 cpus admits 12 of them in the 48-cpu
// room -- declared sizes summed, as every kind did before learned
// requests -- never the learned request a kind's history gives it.
func TestAdmitOnDeclaredSumsDeclaredSizes(t *testing.T) {
	rt := &Runtime{AdmitOn: AdmitOnDeclared, Scheduler: scheduler.New(scheduler.Capacity{CPUs: 48, MemGB: 1000})}
	const kind = "build"
	// a learned request far below the declared size: admit_on=declared
	// must ignore it
	learnKind(rt, kind, 1)
	n := 0
	for ; ; n++ {
		b := buildBrief(kind, n, 4)
		if _, err := rt.Scheduler.TryTake(rt.leaseAsk(b, leaseOf(b.LeaseID))); err != nil {
			if !errors.Is(err, scheduler.ErrNoRoom) {
				t.Fatal(err)
			}
			break
		}
	}
	if n != 12 {
		t.Fatalf("%d builds admitted of 4 declared cpus each in a 48-cpu room, want 12 (declared sizes summed, not the kind's learned 1-cpu request)", n)
	}
}

// With no AdmitOn set a Runtime behaves as AdmitOnRequests: the zero
// value changes nothing for a Runtime built before this goal.
func TestAdmitOnZeroValueIsRequests(t *testing.T) {
	rt := &Runtime{Scheduler: scheduler.New(scheduler.Capacity{CPUs: 48, MemGB: 1000})}
	const kind = "build"
	learnKind(rt, kind, 1)
	b := buildBrief(kind, 0, 12)
	ask := rt.leaseAsk(b, leaseOf(b.LeaseID))
	if ask.CPUs != 1 {
		t.Fatalf("ask.CPUs = %g with AdmitOn unset, want the learned request 1 (the zero value is AdmitOnRequests)", ask.CPUs)
	}
}

// The reserve's floor (reserveNeed, read by reserveFits/takeReserved)
// is always a kind's learned request, even under v1.admit_on=declared:
// a reserve must never be advertised to Mesh only once it has room for
// a declared figure the kind rarely uses.
func TestAdmitOnDeclaredNeverInflatesTheReserveFloor(t *testing.T) {
	rt := reserveRuntime(t, scheduler.Capacity{})
	rt.AdmitOn = AdmitOnDeclared
	const kind = "validate"
	learnKind(rt, kind, 4)
	b := validationBrief("v", "candidate", "runsheet/runsheet")
	b.Resources = meshapi.Resources{CPUs: 32, MemGB: 64}
	own, err := rt.holdCapacity(context.Background(), b, leaseOf(b.LeaseID))
	if err != nil {
		t.Fatalf("holdCapacity: %v", err)
	}
	defer own.Release()
	rt.mu.Lock()
	need := rt.reserveNeed[1]
	rt.mu.Unlock()
	if need.CPUs != 4 {
		t.Fatalf("the validation reserve's floor is %g cpus, want 4 (the learned request, not the 32-cpu declared size admit_on=declared actually holds)", need.CPUs)
	}
}

// node.yaml's v1.admit_on overrides the flag's default; any other
// value is refused.
func TestReadAdmitOn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "node.yaml")
	if got, err := ReadAdmitOn(p, AdmitOnRequests); err != nil || got != AdmitOnRequests {
		t.Fatalf("no file: %v %v", got, err)
	}
	os.WriteFile(p, []byte("v1:\n  admit_on: declared\n"), 0o600)
	if got, err := ReadAdmitOn(p, AdmitOnRequests); err != nil || got != AdmitOnDeclared {
		t.Fatalf("read: %v %v", got, err)
	}
	os.WriteFile(p, []byte("v1:\n  admit_on: sideways\n"), 0o600)
	if _, err := ReadAdmitOn(p, AdmitOnRequests); err == nil {
		t.Fatal("an unknown v1.admit_on value: no error")
	}
}
