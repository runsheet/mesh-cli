package nodev1

import (
	"path"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// A job is sized by its request and its limit (goal
// a-job-is-sized-by-request-and-limit): the brief's declared resources
// stay the lease's hard cap -- what the sandbox limits its containers
// to (limitsOf(b.Resources), unchanged) -- but what the node holds of
// its own scheduler for it, its request, is learned from the kind's
// measured peaks (usage.go) once there are enough of them to trust,
// rather than the declared figure every time: most leases of a kind use
// far less than its declared limit, and asking the scheduler for the
// limit in full books room the lease never uses, same as every other
// kind waiting behind it.

// minMeasuredForLearning is how many of a kind's leases must have a
// measured peak (usage.go) before the node trusts their percentiles
// over its declared resources; fewer, and a new lease's request is the
// declared figure, as it always was.
const minMeasuredForLearning = 20

// requestHeadroom is how far above the measured 95th percentile memory
// peak a learned request asks: a memory overrun is killed, so the
// request keeps headroom past what was ever seen. CPU is only
// throttled past its request, never killed, so its 75th percentile
// alone -- no headroom -- is the request.
const requestHeadroom = 0.25

// requestFloorCPUs and requestFloorMemGB are the least a learned
// request ever asks for, however low a kind's measured peaks run: room
// enough that a lease of it is never scheduled as if it were free.
const (
	requestFloorCPUs  = 0.25
	requestFloorMemGB = 0.5
)

// learnedRequest is kind's request: limit, the declared resources
// (and the lease's hard cap), until measured of its leases have run
// minMeasuredForLearning; then cpuP75 for CPUs and memP95 plus
// requestHeadroom for memory -- each never above limit, nor below the
// floor (requestFloorCPUs, requestFloorMemGB).
func learnedRequest(limit meshapi.Resources, cpuP75, memP95 float64, measured int) meshapi.Resources {
	if measured < minMeasuredForLearning {
		return limit
	}
	req := meshapi.Resources{CPUs: cpuP75, MemGB: memP95 * (1 + requestHeadroom)}
	if limit.CPUs > 0 {
		req.CPUs = min(req.CPUs, limit.CPUs)
	}
	if limit.MemGB > 0 {
		req.MemGB = min(req.MemGB, limit.MemGB)
	}
	req.CPUs = max(req.CPUs, requestFloorCPUs)
	req.MemGB = max(req.MemGB, requestFloorMemGB)
	return req
}

// leaseRequest is b's lease's request (learnedRequest), from its
// kind's measured peaks so far.
func (r *Runtime) leaseRequest(b *meshapi.Brief) meshapi.Resources {
	cpuP75, memP95, measured := r.usage.kindPercentiles(path.Base(b.Kind))
	return learnedRequest(b.Resources, cpuP75, memP95, measured)
}
