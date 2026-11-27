package nodev1

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/scheduler"
)

// builds admits builds of kind "build", declared at declaredCPUs, on a
// 48-cpu node until one does not fit, and answers how many were.
func admitBuilds(t *testing.T, r *Runtime, declaredCPUs float64) int {
	t.Helper()
	n := 0
	for ; n < 1000; n++ {
		b := &meshapi.Brief{LeaseID: fmt.Sprintf("build-%d", n), Kind: "runsheet/build", Resources: meshapi.Resources{CPUs: declaredCPUs, MemGB: 1}}
		if _, err := r.Scheduler.TryTake(r.leaseAsk(b, leaseOf(b.LeaseID))); err != nil {
			if !errors.Is(err, scheduler.ErrNoRoom) {
				t.Fatal(err)
			}
			return n
		}
	}
	return n
}

// requestRuntime is a 48-cpu node whose build kind has 25 measured
// leases peaking at 1 cpu: a learned CPU request of 1.
func requestRuntime(admitOn string) *Runtime {
	r := &Runtime{Scheduler: scheduler.New(scheduler.Capacity{CPUs: 48, MemGB: 400}), AdmitOn: admitOn}
	for i := 0; i < 25; i++ {
		r.usage.ended("build", leasePeaks{read: true, cpu: 1, mem: 1 << 30})
	}
	return r
}

// Goal a-job-is-sized-by-request-and-limit, item 3, acceptance check 1:
// with v1.admit_on requests, a 48-cpu room and builds whose request is
// 1 cpu and limit 12, at least 40 builds are admitted (requests summed,
// limits not), and a build whose request is 9 cpus then waits.
func TestAdmitOnRequestsSumsRequests(t *testing.T) {
	r := requestRuntime(AdmitOnRequests)
	n := admitBuilds(t, r, 12)
	if n < 40 {
		t.Fatalf("%d builds admitted on requests, want at least 40: limits were summed", n)
	}
	// the room is full now; a 9-cpu ask of an unlearned kind waits
	big := &meshapi.Brief{LeaseID: "big", Kind: "runsheet/validate", Resources: meshapi.Resources{CPUs: 9, MemGB: 1}}
	if _, err := r.Scheduler.TryTake(r.leaseAsk(big, leaseOf("big"))); !errors.Is(err, scheduler.ErrNoRoom) {
		t.Fatalf("a 9-cpu request with %d builds holding the room: err %v, want it to wait", n, err)
	}
	// with 40 admitted, 8 cpus are left: still too few for 9
	r2 := requestRuntime(AdmitOnRequests)
	for i := 0; i < 40; i++ {
		b := &meshapi.Brief{LeaseID: fmt.Sprintf("b%d", i), Kind: "runsheet/build", Resources: meshapi.Resources{CPUs: 12, MemGB: 1}}
		if _, err := r2.Scheduler.TryTake(r2.leaseAsk(b, leaseOf(b.LeaseID))); err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
	}
	if _, err := r2.Scheduler.TryTake(r2.leaseAsk(big, leaseOf("big"))); !errors.Is(err, scheduler.ErrNoRoom) {
		t.Fatalf("a 9-cpu request beside 40 1-cpu builds on 48: err %v, want it to wait", err)
	}
}

// Item 3, acceptance check 2: the same fixture with v1.admit_on declared
// (the default) and each build declared at 4 cpus admits 12, as today:
// declared sizes summed, the learned request not used.
func TestAdmitOnDeclaredSumsDeclaredSizes(t *testing.T) {
	for _, on := range []string{AdmitOnDeclared, ""} {
		if n := admitBuilds(t, requestRuntime(on), 4); n != 12 {
			t.Fatalf("admit_on %q: %d builds admitted, want 12 (48 cpus / 4 declared)", on, n)
		}
	}
}

// node.yaml's v1.admit_on wins over the flag; anything but declared or
// requests is refused.
func TestReadAdmitOn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "node.yaml")
	if got, err := ReadAdmitOn(p, AdmitOnDeclared); err != nil || got != AdmitOnDeclared {
		t.Fatalf("no file: %q %v", got, err)
	}
	os.WriteFile(p, []byte("v1:\n  admit_on: requests\n"), 0o600)
	if got, err := ReadAdmitOn(p, AdmitOnDeclared); err != nil || got != AdmitOnRequests {
		t.Fatalf("node.yaml requests: %q %v", got, err)
	}
	os.WriteFile(p, []byte("v1:\n  admit_on: limits\n"), 0o600)
	if _, err := ReadAdmitOn(p, AdmitOnDeclared); err == nil {
		t.Fatal("an unknown admit_on was taken")
	}
}
