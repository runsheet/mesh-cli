package nodev1

import "fmt"

// DiskProbe reads how much free space a path's filesystem has. A
// failed read never gates: the node keeps its last judgement rather
// than guess from an error.
type DiskProbe interface {
	FreeBytes(path string) (uint64, error)
}

// diskGate is "" when the node may take a new lease, else why it may
// not: Disk reads less free space at the build cache's own path (or
// WorkRoot, with no BuildCache set) than the live floor —
// r.buildCacheConfig's MinFreeGB, kept current by WatchBuildCache off
// node.yaml's `cache.min_free_gb` (ADR-044 amendment, goal
// a-nodes-build-cache-stays-under-its-size). Checked once per pass
// through Run's take loop, right after hostGate and before
// r.API.Take — never against a lease already running, which is left
// to finish whatever the disk does meanwhile. Nil Disk, or a floor of
// 0 (no cache.min_free_gb read yet), never gates.
func (r *Runtime) diskGate() string {
	if r.Disk == nil {
		return ""
	}
	r.mu.Lock()
	floorGB := r.buildCacheConfig.MinFreeGB
	r.mu.Unlock()
	if floorGB <= 0 {
		return ""
	}
	path := r.diskGatePath()
	free, err := r.Disk.FreeBytes(path)
	if err != nil {
		return ""
	}
	floor := uint64(floorGB * (1 << 30))
	if free < floor {
		return fmt.Sprintf("disk low at %s: %.1f GB free, floor %.1f GB", path, float64(free)/(1<<30), floorGB)
	}
	return ""
}

// diskGatePath is the path diskGate and the build cache's own default
// (ReadBuildCacheConfig) read free space at: BuildCache, else
// WorkRoot.
func (r *Runtime) diskGatePath() string {
	if r.BuildCache != "" {
		return r.BuildCache
	}
	return r.WorkRoot
}

// noteDiskGate logs a crossing only — entering a reason, or leaving
// one ("") — never the same state twice running.
func (r *Runtime) noteDiskGate(reason string) {
	r.mu.Lock()
	changed := reason != r.diskGateReason
	r.diskGateReason = reason
	r.mu.Unlock()
	if !changed {
		return
	}
	if reason == "" {
		r.logf("nodev1: disk ok; taking leases again")
		return
	}
	r.logf("nodev1: %s; taking no new lease", reason)
}
