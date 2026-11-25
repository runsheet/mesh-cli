package nodev1

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// fakePeakSeries is 25 CPU (cores) and memory (GiB) peak readings whose
// 75th and 95th percentiles (usage.go's percentile, nearest rank) are
// exactly 1.2 cpus and 4.5 GiB: each built strictly ascending so sorting
// leaves them where they were built, with the wanted value at the rank
// percentile reads for n=25 (index 18 for 0.75, index 23 for 0.95).
func fakePeakSeries() (cpu, memGiB []float64) {
	cpu = make([]float64, 25)
	for i := range cpu {
		switch {
		case i < 18:
			cpu[i] = 0.5 + 0.01*float64(i)
		case i == 18:
			cpu[i] = 1.2
		default:
			cpu[i] = 1.3 + 0.01*float64(i-19)
		}
	}
	memGiB = make([]float64, 25)
	for i := range memGiB {
		switch {
		case i < 23:
			memGiB[i] = 1.0 + 0.1*float64(i)
		case i == 23:
			memGiB[i] = 4.5
		default:
			memGiB[i] = 5.0
		}
	}
	return cpu, memGiB
}

// Goal a-job-is-sized-by-request-and-limit, acceptance check 1: 25
// ended build leases with fake peaks whose CPU p75 is 1.2 and memory
// p95 is 4.5 GB give a new build lease a learned request of 1.2 cpus
// and 5.625 GiB (p95 plus requestHeadroom's 25%) -- the lease's own
// hard cap, its declared resources, stays what it was.
func TestLearnedRequestFromPeakHistory(t *testing.T) {
	r := &Runtime{}
	const kind = "build"
	cpu, mem := fakePeakSeries()
	for i := range cpu {
		r.usage.ended(kind, leasePeaks{read: true, cpu: cpu[i], mem: uint64(mem[i] * (1 << 30))})
	}
	b := &meshapi.Brief{Kind: kind, Resources: meshapi.Resources{CPUs: 4, MemGB: 16}}
	req := r.leaseRequest(b)
	if req.CPUs != 1.2 {
		t.Fatalf("request CPUs = %g, want the measured 75th percentile 1.2", req.CPUs)
	}
	if req.MemGB != 5.625 {
		t.Fatalf("request MemGB = %g, want the measured 95th percentile 4.5 plus 25%% headroom, 5.625", req.MemGB)
	}
	// admitted on requests (v1.admit_on), the lease's own hold is that
	// CPU request; its memory stays the declared size in this first cut
	r.AdmitOn = AdmitOnRequests
	if ask := r.leaseAsk(b, leaseOf("new-build")); ask.CPUs != 1.2 || ask.MemGB != 16 {
		t.Fatalf("admit_on=requests: ask %g cpus, %g GB; want 1.2 cpus, the declared 16 GB", ask.CPUs, ask.MemGB)
	}
}

// Goal a-job-is-sized-by-request-and-limit, acceptance check 2: a kind
// with fewer than minMeasuredForLearning measured leases gives a new
// lease of it the brief's declared resources as its request, not a
// learned one -- there is not enough history to trust yet.
func TestLearnedRequestFallsBackWithFewMeasuredLeases(t *testing.T) {
	r := &Runtime{}
	const kind = "small"
	for i := 0; i < minMeasuredForLearning-1; i++ {
		r.usage.ended(kind, leasePeaks{read: true, cpu: 9, mem: 9 << 30})
	}
	b := &meshapi.Brief{Kind: kind, Resources: meshapi.Resources{CPUs: 3, MemGB: 6}}
	r.AdmitOn = AdmitOnRequests
	ask := r.leaseAsk(b, leaseOf("new-small"))
	if ask.CPUs != 3 || ask.MemGB != 6 {
		t.Fatalf("ask = %g cpus, %g GB; want the declared 3 cpus, 6 GB (only %d measured, fewer than %d)",
			ask.CPUs, ask.MemGB, minMeasuredForLearning-1, minMeasuredForLearning)
	}
}

// learnedRequest itself never asks for more than the kind's declared
// limit, however high its measured peaks run, nor for less than the
// floor, however low they run.
func TestLearnedRequestNeverAboveLimitOrBelowFloor(t *testing.T) {
	limit := meshapi.Resources{CPUs: 1, MemGB: 2}
	if got := learnedRequest(limit, 10, 10, minMeasuredForLearning); got != limit {
		t.Fatalf("learnedRequest with peaks far past the limit = %+v, want the limit %+v", got, limit)
	}
	roomy := meshapi.Resources{CPUs: 8, MemGB: 16}
	want := meshapi.Resources{CPUs: requestFloorCPUs, MemGB: requestFloorMemGB}
	if got := learnedRequest(roomy, 0.01, 0.01, minMeasuredForLearning); got != want {
		t.Fatalf("learnedRequest with peaks far below the floor = %+v, want the floor %+v", got, want)
	}
}

// Goal a-job-is-sized-by-request-and-limit, acceptance check 3: `mesh
// node status` reports a running lease's limit (its declared, hard-cap
// resources), its request (learned from its kind's peak history), and
// its now and peak container readings (usage.go).
func TestStatusLeaseLineReportsLimitRequestNowAndPeak(t *testing.T) {
	r := &Runtime{}
	const kind = "build"
	cpu, mem := fakePeakSeries()
	for i := range cpu {
		r.usage.ended(kind, leasePeaks{read: true, cpu: cpu[i], mem: uint64(mem[i] * (1 << 30))})
	}
	b := &meshapi.Brief{Kind: kind, Resources: meshapi.Resources{CPUs: 4, MemGB: 16}}
	r.leases.Put("lease-1", &running{
		jobID: "job-1", kind: kind, started: time.Now(), brief: b,
		peaks: leasePeaks{read: true, cpu: 0.9, mem: 3 << 30, nowCPU: 0.5, nowMem: 2 << 30},
	})

	st := r.Status()
	if len(st.Leases) != 1 {
		t.Fatalf("%d leases, want 1", len(st.Leases))
	}
	l := st.Leases[0]
	if l.LimitCPUs != 4 || l.LimitMemGB != 16 {
		t.Fatalf("limit = %g cpus, %g GB, want the declared 4, 16", l.LimitCPUs, l.LimitMemGB)
	}
	if l.RequestCPUs != 1.2 || l.RequestMemGB != 5.625 {
		t.Fatalf("request = %g cpus, %g GB, want the learned 1.2, 5.625", l.RequestCPUs, l.RequestMemGB)
	}
	if l.NowCPUs != 0.5 || l.NowMemGB != 2 {
		t.Fatalf("now = %g cpus, %g GB, want 0.5, 2", l.NowCPUs, l.NowMemGB)
	}
	if l.PeakCPUs != 0.9 || l.PeakMemGB != 3 {
		t.Fatalf("peak = %g cpus, %g GB, want 0.9, 3", l.PeakCPUs, l.PeakMemGB)
	}

	lines := strings.Join(st.DrainLines(time.Now()), "\n")
	for _, want := range []string{
		fmt.Sprintf("limit %.2f cpus/%.2f GB", 4.0, 16.0),
		fmt.Sprintf("request %.2f cpus/%.2f GB", 1.2, 5.625),
		fmt.Sprintf("now %.2f cpus/%.2f GB", 0.5, 2.0),
		fmt.Sprintf("peak %.2f cpus/%.2f GB", 0.9, 3.0),
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("status lines\n%s\nlack %q", lines, want)
		}
	}
}
